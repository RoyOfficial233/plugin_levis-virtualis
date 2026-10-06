package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/pkg/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestPluginProcessMain(t *testing.T) {
	if os.Getenv("LEVIS_TEST_PLUGIN_PROCESS") != "1" {
		t.Skip("subprocess helper")
	}
	main()
}

// Run the actual main server wiring, not a differently configured test server.
func TestPluginStreamingRPCRequiresToken(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "archive")
	}))
	defer srv.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPluginProcessMain$")
	cmd.Env = append(os.Environ(), "LEVIS_TEST_PLUGIN_PROCESS=1", plugin.EnvToken+"=server-token")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("missing handshake: %v", scanner.Err())
	}
	var handshake struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &handshake); err != nil || handshake.Port == 0 {
		t.Fatalf("invalid handshake: %s", scanner.Text())
	}
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", handshake.Port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewPluginClient(conn)
	req := &pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "upstream-key")}
	for _, token := range []string{"", "wrong-token", "server-token"} {
		callCtx := ctx
		if token != "" {
			callCtx = metadata.AppendToOutgoingContext(ctx, plugin.MetadataToken, token)
		}
		stream, err := client.DownloadHostBackup(callCtx, req)
		if err != nil {
			t.Fatal(err)
		}
		chunk, err := stream.Recv()
		if token != "server-token" {
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("unauthenticated streaming request returned chunk=%v err=%v", chunk, err)
			}
		} else {
			if err != nil || string(chunk.GetData()) != "archive" {
				t.Fatalf("authenticated stream: %v %v", chunk, err)
			}
			if _, err = stream.Recv(); err != io.EOF {
				t.Fatalf("stream did not finish: %v", err)
			}
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("unauthorized requests reached upstream: %d", requests.Load())
	}
}

func TestBackupDownloadRejectsTruncatedChunkedTransfer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n20\r\nshort")
		rw.Flush()
	}))
	defer srv.Close()
	stream := &backupStream{ctx: context.Background()}
	err := (&virtualisPlugin{client: srv.Client()}).DownloadHostBackup(&pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "key")}, stream)
	if status.Code(err) != codes.DataLoss || len(stream.chunks) != 0 {
		t.Fatalf("truncated chunked transfer: err=%v chunks=%d", err, len(stream.chunks))
	}
}

func TestBackupDownloadSizeContentEncodingAndRedirectGuards(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	for _, tc := range []struct {
		name    string
		headers map[string]string
		status  int
		code    codes.Code
	}{
		{"too_large", map[string]string{"Content-Length": fmt.Sprint(int64(64<<30) + 1)}, 200, codes.ResourceExhausted},
		{"encoded", map[string]string{"Content-Encoding": "gzip"}, 200, codes.DataLoss},
		{"redirect", map[string]string{"Location": target.URL}, 307, codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("archive bytes may be transparently decompressed")
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "archive")
			}))
			defer srv.Close()
			stream := &backupStream{ctx: context.Background()}
			err := (&virtualisPlugin{client: srv.Client()}).DownloadHostBackup(&pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "key")}, stream)
			if status.Code(err) != tc.code || len(stream.chunks) != 0 {
				t.Fatalf("guard: %v chunks=%d", err, len(stream.chunks))
			}
		})
	}
	if leaked.Load() {
		t.Fatal("redirect leaked credentials")
	}
}

func TestBackupDownloadCancelsBlockedUpstream(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- (&virtualisPlugin{client: srv.Client()}).DownloadHostBackup(&pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "key")}, &backupStream{ctx: ctx})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("cancel=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked read ignored cancellation")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream body not closed")
	}
}

func TestBackupFilenameNeverExposesPathsOrControlCharacters(t *testing.T) {
	for _, name := range []string{"../../secret.tar", `C:\secrets.tar`, ".hidden.tar", "bad\r\nheader.tar", "backup.exe", "backup.tar"} {
		got := backupFilename(fmt.Sprintf("attachment; filename=%q", name), "application/octet-stream", 4)
		if strings.ContainsAny(got, "/\\:\r\n") || strings.HasPrefix(got, ".") {
			t.Fatalf("unsafe filename=%q", got)
		}
		if name == "backup.tar" && got != name {
			t.Fatal("safe filename lost")
		}
	}
}
