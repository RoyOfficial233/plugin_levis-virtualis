package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type backupStream struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*pb.HostBackupChunk
	fail   error
}

func (s *backupStream) Context() context.Context { return s.ctx }
func (s *backupStream) Send(c *pb.HostBackupChunk) error {
	if s.fail != nil {
		return s.fail
	}
	s.chunks = append(s.chunks, &pb.HostBackupChunk{Data: append([]byte(nil), c.GetData()...), Filename: c.GetFilename()})
	return nil
}

func TestDownloadHostBackupStreamsBoundedChunksAndSafeFilename(t *testing.T) {
	archive := bytes.Repeat([]byte("actual-stream-test-bytes"), 10000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/instances/7/backups/4/download" {
			t.Errorf("route=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Virtualis-Api-Key") != "download-key" {
			t.Error("credentials missing")
		}
		w.Header().Set("Content-Disposition", `attachment; filename="backup-4.tar.gz"`)
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(archive)
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	stream := &backupStream{ctx: context.Background()}
	err := p.DownloadHostBackup(&pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "download-key")}, stream)
	if err != nil {
		t.Fatal(err)
	}
	var combined []byte
	for i, chunk := range stream.chunks {
		if len(chunk.GetData()) > 64<<10 || len(chunk.GetData()) == 0 {
			t.Fatal("invalid chunk size")
		}
		if i < len(stream.chunks)-1 && len(chunk.GetData()) != 64<<10 {
			t.Fatal("short middle chunk")
		}
		combined = append(combined, chunk.GetData()...)
	}
	if len(stream.chunks) < 2 || stream.chunks[0].GetFilename() != "backup-4.tar.gz" || !bytes.Equal(combined, archive) {
		t.Fatal("streamed bytes/filename mismatch")
	}
}

func TestBackupDownloadErrorsAreNotSuccessfulArchives(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		status                         int
		contentType, disposition, body string
		code                           codes.Code
	}{
		{"resource_missing", 404, "application/json", "", `{"code":"NOT_FOUND","message":"missing"}`, codes.NotFound},
		{"route_missing", 404, "text/plain", "", "404 page not found", codes.Unimplemented},
		{"structured_route_missing", 404, "application/json", "", `{"code":"NOT_FOUND","message":"not found"}`, codes.Unimplemented},
		{"permission", 403, "application/json", "", `{"code":"FORBIDDEN","message":"denied"}`, codes.PermissionDenied},
		{"json_success", 200, "application/json", "", `{"error":"wrong handler"}`, codes.DataLoss},
		{"empty", 200, "application/octet-stream", "", "", codes.DataLoss},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Disposition", tc.disposition)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			p := &virtualisPlugin{client: srv.Client()}
			stream := &backupStream{ctx: context.Background()}
			err := p.DownloadHostBackup(&pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "download-key")}, stream)
			if status.Code(err) != tc.code || len(stream.chunks) > 0 {
				t.Fatalf("error=%v chunks=%d", err, len(stream.chunks))
			}
		})
	}
}

func TestBackupDownloadValidatesCredentialsIDsAndFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../credentials.tar"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "archive")
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	for _, req := range []*pb.HostBackupRequest{{HostId: "7", BackupId: 4}, {HostId: "../7", BackupId: 4, InterfaceConfig: testConfig(srv, "key")}, {HostId: "7", BackupId: 0, InterfaceConfig: testConfig(srv, "key")}} {
		if err := p.DownloadHostBackup(req, &backupStream{ctx: context.Background()}); err == nil {
			t.Fatal("invalid download accepted")
		}
	}
	stream := &backupStream{ctx: context.Background()}
	if err := p.DownloadHostBackup(&pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "key")}, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.chunks) != 1 || strings.ContainsAny(stream.chunks[0].GetFilename(), "/\\:") {
		t.Fatal("unsafe filename exposed")
	}
}

func TestBackupDownloadCancellationAndSendFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "archive")
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	req := &pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "key")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.DownloadHostBackup(req, &backupStream{ctx: ctx}); status.Code(err) != codes.Canceled {
		t.Fatalf("cancel error=%v", err)
	}
	sentinel := errors.New("downstream disconnected")
	if err := p.DownloadHostBackup(req, &backupStream{ctx: context.Background(), fail: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("send error=%v", err)
	}
}

func TestBackupDownloadDetectsTruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, "short")
	}))
	defer srv.Close()
	p := &virtualisPlugin{client: srv.Client()}
	err := p.DownloadHostBackup(&pb.HostBackupRequest{HostId: "7", BackupId: 4, InterfaceConfig: testConfig(srv, "key")}, &backupStream{ctx: context.Background()})
	if status.Code(err) != codes.DataLoss {
		t.Fatalf("truncation error=%v", err)
	}
}
