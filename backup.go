package main

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	backupChunkSize       = 64 << 10
	maxBackupBytes  int64 = 64 << 30
)

var backupFilenamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func backupFilename(disposition, mediaType string, id uint64) string {
	kind, params, err := mime.ParseMediaType(disposition)
	name := params["filename"]
	if err == nil && kind == "attachment" && backupFilenamePattern.MatchString(name) &&
		(strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz")) {
		return name
	}
	ext := ".tar"
	if mediaType == "application/gzip" || mediaType == "application/x-gzip" {
		ext += ".gz"
	}
	return "backup-" + decimalID(id) + ext
}

func downloadError(err error, ctx context.Context) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	if unsupportedRoute(err) || structuredRouteMissing(err) {
		return status.Error(codes.Unimplemented, "上游不支持备份下载端点")
	}
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		switch upstream.HTTPStatus {
		case http.StatusNotFound:
			return status.Error(codes.NotFound, "备份或实例不存在")
		case http.StatusUnauthorized:
			return status.Error(codes.Unauthenticated, "上游 API 密钥无效")
		case http.StatusForbidden:
			return status.Error(codes.PermissionDenied, "上游拒绝备份下载")
		case http.StatusConflict:
			return status.Error(codes.FailedPrecondition, "备份当前不可下载")
		case http.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, "上游请求限流")
		}
	}
	// Do not return URL-bearing net/http errors or upstream error bodies.
	return status.Error(codes.Unavailable, "上游备份下载失败")
}

// Fill a fixed-size chunk while preserving the body's own error. ReadFull
// would conflate a legitimate short final chunk with truncated HTTP chunking.
func readBackupChunk(body io.Reader, buffer []byte) (int, error) {
	total, empty := 0, 0
	for total < len(buffer) {
		n, err := body.Read(buffer[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			empty++
			if empty >= 100 {
				return total, io.ErrNoProgress
			}
		} else {
			empty = 0
		}
	}
	return total, nil
}

// DownloadHostBackup applies backpressure all the way to the HTTP body. Only
// one fixed-size buffer is held, never an archive or a temporary archive file.
func (p *virtualisPlugin) DownloadHostBackup(req *pb.HostBackupRequest, stream grpc.ServerStreamingServer[pb.HostBackupChunk]) error {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return err
	}
	if !validID(req.GetHostId()) || req.GetBackupId() == 0 {
		return status.Error(codes.InvalidArgument, "实例和备份 ID 必须是正整数")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), recoveryTimeout)
	defer cancel()
	path := "/instances/" + req.GetHostId() + "/backups/" + decimalID(req.GetBackupId()) + "/download"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, creds.apiURL+"/api/v1"+path, nil)
	if err != nil {
		return status.Error(codes.FailedPrecondition, "无法构造上游下载请求")
	}
	httpReq.Header.Set("X-Virtualis-Api-Key", creds.apiKey)
	// Prevent transparent gzip decompression from changing archive bytes or
	// obscuring the Content-Length used for truncation checks.
	httpReq.Header.Set("Accept-Encoding", "identity")
	resp, err := p.requestClient(recoveryTimeout).Do(httpReq)
	if err != nil {
		return downloadError(err, ctx)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponse+1))
		if readErr != nil {
			return downloadError(readErr, ctx)
		}
		return downloadError(parseUpstreamError(resp, raw, creds), ctx)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return status.Error(codes.DataLoss, "上游未返回备份文件类型")
	}
	switch mediaType {
	case "application/octet-stream", "application/x-tar", "application/gzip", "application/x-gzip":
	default:
		return status.Error(codes.DataLoss, "上游返回的不是备份归档")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return status.Error(codes.DataLoss, "上游不应对备份应用 HTTP 内容编码")
	}
	if resp.ContentLength > maxBackupBytes {
		return status.Error(codes.ResourceExhausted, "备份超过 64 GiB 限制")
	}
	if resp.ContentLength == 0 {
		return status.Error(codes.DataLoss, "上游备份为空")
	}
	filename := backupFilename(resp.Header.Get("Content-Disposition"), mediaType, req.GetBackupId())
	buffer := make([]byte, backupChunkSize)
	var total int64
	for {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		n, readErr := readBackupChunk(resp.Body, buffer)
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if int64(n) > maxBackupBytes-total {
			return status.Error(codes.ResourceExhausted, "备份超过 64 GiB 限制")
		}
		total += int64(n)
		if readErr != nil && readErr != io.EOF {
			return status.Error(codes.DataLoss, "读取上游备份失败")
		}
		if resp.ContentLength >= 0 && (total > resp.ContentLength || (readErr != nil && total != resp.ContentLength)) {
			return status.Error(codes.DataLoss, "上游备份传输被截断或长度不符")
		}
		if n > 0 {
			chunk := &pb.HostBackupChunk{Data: buffer[:n]}
			if total == int64(n) {
				chunk.Filename = filename
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		if readErr != nil {
			if total == 0 {
				return status.Error(codes.DataLoss, "上游备份为空")
			}
			return nil
		}
	}
}
