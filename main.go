// Virtualis 对接插件：使 Levis（下游销售面板）可以对接上游 Virtualis
// 弹性云主机系统，实现「接口 + 商品配置 + 下单开通 + 电源管理」。
//
// 与传统上游插件（如魔方财务）不同，本插件没有全局配置 —— Levis 后台
// 「接口管理」可以为同一个模块配置多个接口（不同的 Virtualis 站点地址
// 与站点 API Key），配置经每个 RPC 的 interface_config 按请求透传：
//
//	api_url  上游 Virtualis 主控地址，如 http://114.66.41.15:8090
//	api_key  Virtualis 后台生成的站点 API 密钥（X-Virtualis-Api-Key）
//
// 依赖上游 /api/v1 机器 API（API Key 鉴权）：
//
//	GET    /api/v1/images?driver=…   镜像列表（购买页选系统）
//	POST   /api/v1/instances         创建实例（agent_id 缺省自动选节点）
//	GET    /api/v1/instances/:id     实例详情
//	DELETE /api/v1/instances/:id     删除实例
//	POST   /api/v1/instances/:id/power 电源操作（start/stop/restart）
//
//	POST   /api/v1/instances/:id/vnc-ticket VNC 一次性短票（GetHostVNC 用）
//
// 购买选配（弹性云）经 CreateOrder 的 options 传入：
//
//	driver（incus/qemu）、cpu、memory_mb、disk_gb、bandwidth_mbps、
//	traffic_gb（上游暂不计量，仅快照展示）、image_id、image_name。
//
// 售后流量加购（流量包）不在上游落地：配额只在 Levis 本地 Service.traffic_extra_gb
// 累加；Levis 结清后会以 ManageHost(action=UNSPECIFIED, os="traffic_gb=N") 做
// best-effort 通知，插件侧仅记录并返回成功（见 ManageHost）。
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/SakuraOpenSource/levis/pkg/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

const (
	pluginName    = "Virtualis 对接"
	pluginVersion = "1.0.0"
	pluginDesc    = "对接上游 Virtualis 弹性云主机系统：弹性/固定配置商品下单开通、系统镜像选择与电源管理（经接口管理配置站点地址与密钥）"
)

func main() {
	token := os.Getenv(plugin.EnvToken)
	if token == "" {
		fmt.Fprintln(os.Stderr, "缺少令牌")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "监听失败:", err)
		os.Exit(1)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(token)))
	srv := &virtualisPlugin{server: server, client: &http.Client{Timeout: 60 * time.Second}}
	pb.RegisterPluginServer(server, srv)

	port := listener.Addr().(*net.TCPAddr).Port
	line, _ := json.Marshal(map[string]int{"port": port})
	fmt.Println(string(line))

	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, "服务退出:", err)
	}
}

func authInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context, req any,
		info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "缺少令牌")
		}
		values := md.Get(plugin.MetadataToken)
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, "缺少令牌")
		}
		if subtle.ConstantTimeCompare([]byte(values[0]), []byte(token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "令牌不匹配")
		}
		return handler(ctx, req)
	}
}

type virtualisPlugin struct {
	pb.UnimplementedPluginServer
	server *grpc.Server

	mu     sync.Mutex
	client *http.Client
}

// =============================================================================
// 核心生命周期方法
// =============================================================================

func (p *virtualisPlugin) Describe(context.Context, *pb.DescribeRequest) (*pb.Manifest, error) {
	return &pb.Manifest{
		Name:        pluginName,
		Version:     pluginVersion,
		Description: pluginDesc,
		Author:      "Levis Team",
		Capabilities: []pb.Capability{
			pb.Capability_CAPABILITY_PROVISION_PRODUCT,
		},
		// 配置字段由「接口管理」按接口填写并按请求透传，插件进程不持有。
		Config: []*pb.ConfigField{
			{
				Key:      "api_url",
				Label:    "Virtualis 地址",
				Type:     pb.FieldType_FIELD_TYPE_TEXT,
				Required: true,
				Hint:     "上游 Virtualis 主控的完整地址，如 http://114.66.41.15:8090",
			},
			{
				Key:      "api_key",
				Label:    "API 密钥",
				Type:     pb.FieldType_FIELD_TYPE_TEXT,
				Required: true,
				Secret:   true,
				Hint:     "Virtualis 后台「安全 → API 密钥」生成的站点密钥",
			},
		},
	}, nil
}

// Configure 刷新配置。全局配置恒为空（凭据按请求透传），但主程序在启动与
// 重配时都会调用，必须友好接收。
func (p *virtualisPlugin) Configure(_ context.Context, req *pb.ConfigureRequest) (*pb.ConfigureReply, error) {
	_ = req.GetValues()
	return &pb.ConfigureReply{}, nil
}

func (p *virtualisPlugin) Health(context.Context, *pb.HealthRequest) (*pb.HealthReply, error) {
	return &pb.HealthReply{Ok: true}, nil
}

func (p *virtualisPlugin) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownReply, error) {
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.server.GracefulStop()
	}()
	return &pb.ShutdownReply{}, nil
}

// =============================================================================
// 上游 HTTP 访问
// =============================================================================

// credentials 是一次调用所需的上游地址与密钥，取自请求的 interface_config。
type credentials struct {
	apiURL string
	apiKey string
}

// credsFromConfig 从接口配置里取出上游地址与密钥。
func credsFromConfig(config map[string]string) (*credentials, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(config["api_url"]), "/")
	apiKey := strings.TrimSpace(config["api_key"])
	if apiURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "接口未配置 Virtualis 地址（api_url）")
	}
	if apiKey == "" {
		return nil, status.Error(codes.FailedPrecondition, "接口未配置 API 密钥（api_key）")
	}
	return &credentials{apiURL: apiURL, apiKey: apiKey}, nil
}

// apiGet 调用上游 GET /api/v1 接口并把 data 解到 out。
func (p *virtualisPlugin) apiGet(ctx context.Context, creds *credentials, path string, out any) error {
	return p.apiDo(ctx, http.MethodGet, creds, path, nil, out)
}

// apiPost 调用上游 POST /api/v1 接口。
func (p *virtualisPlugin) apiPost(ctx context.Context, creds *credentials, path string, body any, out any) error {
	return p.apiDo(ctx, http.MethodPost, creds, path, body, out)
}

func (p *virtualisPlugin) apiDo(ctx context.Context, method string, creds *credentials, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求失败: %w", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, creds.apiURL+"/api/v1"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Virtualis-Api-Key", creds.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("上游请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("读取上游响应失败: %w", err)
	}

	// Virtualis 是 REST 风格：所有 2xx 都表示成功，失败 4xx/5xx 给
	// {code, message}。204 等无正文响应直接成功。
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var errBody struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &errBody) == nil && errBody.Message != "" {
			return fmt.Errorf("上游返回错误: %s", errBody.Message)
		}
		return fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	// 204 或 "null"（成功但无数据）都跳过解析。
	if out != nil && len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析上游数据失败: %w，内容: %s", err, truncate(string(raw), 200))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// =============================================================================
// Virtualis 数据形态（/api/v1 所需的最小子集）
// =============================================================================

type v1Image struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Driver      string `json:"driver"`
	Status      string `json:"status"`
}

type v1Spec struct {
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	Arch     string `json:"arch,omitempty"`
}

type v1Network struct {
	Mode          string   `json:"mode"`
	Bridge        string   `json:"bridge,omitempty"`
	MAC           string   `json:"mac,omitempty"`
	IPv4          string   `json:"ipv4,omitempty"`
	Gateway       string   `json:"gateway,omitempty"`
	DNS           []string `json:"dns,omitempty"`
	BandwidthMbps int      `json:"bandwidth_mbps,omitempty"`
}

type v1NetworkStatus struct {
	Reachable  bool                 `json:"reachable"`
	LatencyMS  float64              `json:"latency_ms"`
	Interfaces []v1NetworkInterface `json:"interfaces"`
}

type v1NetworkInterface struct {
	Name string   `json:"name"`
	IPv4 []string `json:"ipv4"`
	IPv6 []string `json:"ipv6"`
	MAC  string   `json:"mac"`
}

type v1Instance struct {
	ID          uint       `json:"id"`
	Name        string     `json:"name"`
	Driver      string     `json:"driver"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	Spec        v1Spec     `json:"spec"`
	Network     v1Network  `json:"network"`
	IP          string     `json:"ip"`
	ObservedIP  string     `json:"observed_ip"`
	SSHPassword string     `json:"ssh_password"`
	SSHReady    bool       `json:"ssh_ready"`
	NATMappings []v1NATMap `json:"nat_mappings"`
	Agent       *v1Agent   `json:"agent"`
}

type v1Agent struct {
	IP string `json:"ip"`
}

type v1NATMap struct {
	Protocol  string `json:"protocol"`
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
}

// =============================================================================
// 产品相关方法
// =============================================================================

// ListProducts 返回空列表：Virtualis 没有上游「产品」概念 —— 商品在
// Levis 本地创建（接口 + 弹性/固定配置），开通时按选配直接建实例。
// 本方法同时兼任接口管理「测试连通」的探针。
func (p *virtualisPlugin) ListProducts(ctx context.Context, req *pb.ListProductsRequest) (*pb.ListProductsReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	// 用一个轻量请求验证地址与密钥可用。
	if _, err := p.listImages(ctx, creds, ""); err != nil {
		return &pb.ListProductsReply{Error: err.Error()}, nil
	}
	return &pb.ListProductsReply{Products: []*pb.UpstreamProduct{}, Total: 0}, nil
}

func (p *virtualisPlugin) GetProduct(context.Context, *pb.GetProductRequest) (*pb.GetProductReply, error) {
	return &pb.GetProductReply{Error: "Virtualis 商品在本地创建，无需从上游同步"}, nil
}

// =============================================================================
// 下单开通
// =============================================================================

// option 把 options 里的字符串解析成非负整数，缺省回退 def。
func optionInt(options map[string]string, key string, def int) int {
	raw := strings.TrimSpace(options[key])
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return def
	}
	return value
}

// instanceName 依据订单号生成上游实例名：小写字母/数字/短横线，
// 且带随机后缀避免同单多台或重开时撞唯一索引。
func instanceName(remark string) string {
	base := strings.ToLower(strings.TrimSpace(remark))
	var kept strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			kept.WriteRune(r)
		case r == '-' || r == '_':
			kept.WriteRune('-')
		}
	}
	name := strings.Trim(kept.String(), "-")
	if name == "" {
		name = "levis"
	}
	return fmt.Sprintf("lv-%s-%04x", name, rand.Intn(0xffff))
}

// listImages 拉取指定驱动的镜像列表。/api/v1/images 返回分页外壳
// {"items":[...]}，这里统一解包。
func (p *virtualisPlugin) listImages(ctx context.Context, creds *credentials, driver string) ([]v1Image, error) {
	var page struct {
		Items []v1Image `json:"items"`
	}
	if err := p.apiGet(ctx, creds, "/images?driver="+driver, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// pickImage 没有指定镜像时按驱动自选一个可用镜像。
func (p *virtualisPlugin) pickImage(ctx context.Context, creds *credentials, driver string) (uint, string, error) {
	images, err := p.listImages(ctx, creds, driver)
	if err != nil {
		return 0, "", err
	}
	for _, image := range images {
		if image.Status == "" || image.Status == "available" {
			return image.ID, image.DisplayName, nil
		}
	}
	return 0, "", fmt.Errorf("驱动 %s 暂无可用镜像，请先在上游下载", driver)
}

// CreateOrder 在上游创建一台实例并返回其 ID 作为上游订单号。
// 实例创建是异步引导的，上游返回即视为开通成功（状态 creating/running）。
func (p *virtualisPlugin) CreateOrder(ctx context.Context, req *pb.CreateOrderRequest) (*pb.CreateOrderReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	options := req.GetOptions()

	driver := strings.ToLower(strings.TrimSpace(options["driver"]))
	if driver != "qemu" {
		driver = "incus"
	}

	cpu := optionInt(options, "cpu", 1)
	memoryMB := optionInt(options, "memory_mb", 512)
	diskGB := optionInt(options, "disk_gb", 10)
	if cpu < 1 {
		return &pb.CreateOrderReply{Error: "CPU 核数必须大于零"}, nil
	}
	if memoryMB < 16 {
		return &pb.CreateOrderReply{Error: "内存必须大于零"}, nil
	}
	if diskGB < 1 {
		return &pb.CreateOrderReply{Error: "硬盘容量必须大于零"}, nil
	}

	imageID := uint(optionInt(options, "image_id", 0))
	if imageID == 0 {
		// 管理员代开等场景没有用户选配：按驱动自选一个可用镜像。
		id, _, err := p.pickImage(ctx, creds, driver)
		if err != nil {
			return &pb.CreateOrderReply{Error: err.Error()}, nil
		}
		imageID = id
	}

	// QEMU 镜像/实例类型为 vm，Incus 为 container；网络统一 NAT 模式。
	body := map[string]any{
		"name":     instanceName(req.GetRemark()),
		"driver":   driver,
		"type":     "container",
		"spec":     v1Spec{CPU: cpu, MemoryMB: memoryMB, DiskGB: diskGB},
		"network":  v1Network{Mode: "nat", BandwidthMbps: optionInt(options, "bandwidth_mbps", 0)},
		"image_id": imageID,
	}
	if driver == "qemu" {
		body["type"] = "vm"
	}
	// 数量由主程序按单逐台调用（Quantity 恒为 1），此处无需展开。

	var instance v1Instance
	if err := p.apiPost(ctx, creds, "/instances", body, &instance); err != nil {
		return &pb.CreateOrderReply{Error: err.Error()}, nil
	}
	if instance.ID == 0 {
		return &pb.CreateOrderReply{Error: "上游未返回实例 ID"}, nil
	}

	name := instance.Name
	if name == "" {
		name = fmt.Sprintf("实例 #%d", instance.ID)
	}
	return &pb.CreateOrderReply{
		UpstreamOrderId: strconv.FormatUint(uint64(instance.ID), 10),
		UpstreamOrderNo: name,
		Status:          "active",
	}, nil
}

// GetOrder 查询实例状态。
func (p *virtualisPlugin) GetOrder(ctx context.Context, req *pb.GetOrderRequest) (*pb.GetOrderReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	var instance v1Instance
	if err := p.apiGet(ctx, creds, "/instances/"+req.GetUpstreamOrderId(), &instance); err != nil {
		return &pb.GetOrderReply{Error: err.Error()}, nil
	}
	return &pb.GetOrderReply{
		UpstreamOrderId: req.GetUpstreamOrderId(),
		UpstreamOrderNo: instance.Name,
		Status:          v1Status(instance.Status),
		HostId:          req.GetUpstreamOrderId(),
	}, nil
}

// v1Status 把上游实例状态映射成 Levis 服务状态口径。
func v1Status(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "running":
		return "active"
	case "stopped":
		return "suspended"
	case "error":
		return "suspended"
	default:
		return "pending"
	}
}

// =============================================================================
// 服务管理
// =============================================================================

func (p *virtualisPlugin) ManageHost(ctx context.Context, req *pb.ManageHostRequest) (*pb.ManageHostReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	hostID := req.GetHostId()

	switch req.GetAction() {
	case pb.HostAction_HOST_ACTION_RENEW:
		// Virtualis 不做上游计费，续费只顺延本地到期时间。
		return &pb.ManageHostReply{Success: true}, nil

	case pb.HostAction_HOST_ACTION_SUSPEND, pb.HostAction_HOST_ACTION_SHUTDOWN:
		return p.power(ctx, creds, hostID, "stop")

	case pb.HostAction_HOST_ACTION_UNSUSPEND, pb.HostAction_HOST_ACTION_BOOT:
		return p.power(ctx, creds, hostID, "start")

	case pb.HostAction_HOST_ACTION_REBOOT:
		return p.power(ctx, creds, hostID, "restart")

	case pb.HostAction_HOST_ACTION_HARD_BOOT:
		return p.power(ctx, creds, hostID, "hard_start")

	case pb.HostAction_HOST_ACTION_HARD_STOP:
		return p.power(ctx, creds, hostID, "hard_stop")

	case pb.HostAction_HOST_ACTION_HARD_RESTART:
		return p.power(ctx, creds, hostID, "hard_restart")

	case pb.HostAction_HOST_ACTION_TERMINATE:
		if err := p.apiDo(ctx, http.MethodDelete, creds, "/instances/"+hostID, nil, nil); err != nil {
			// 删除要幂等：上游已经没有这个实例（重复删除、本地残留补偿）
			// 视为成功，否则本地会永远删不掉。
			if !strings.Contains(err.Error(), "not found") {
				return &pb.ManageHostReply{Error: err.Error()}, nil
			}
		}
		return &pb.ManageHostReply{Success: true}, nil

	case pb.HostAction_HOST_ACTION_REINSTALL:
		osID := strings.TrimSpace(req.GetOs())
		if osID == "" || osID == "0" {
			return &pb.ManageHostReply{Error: "重装系统必须指定镜像 ID"}, nil
		}
		return p.powerWithImage(ctx, creds, hostID, "reinstall", osID)

	default:
		if req.GetAction() == pb.HostAction_HOST_ACTION_UNSPECIFIED {
			if extra, ok := parseTrafficTopUp(req.GetOs()); ok {
				fmt.Fprintf(os.Stderr, "[virtualis] 流量包记录 host=%s extra_gb=%d\n", hostID, extra)
				return &pb.ManageHostReply{Success: true}, nil
			}
		}
		return &pb.ManageHostReply{Error: "不支持的操作类型"}, nil
	}
}

// parseTrafficTopUp 解析流量包加购通知（os 形如 "traffic_gb=100"，单位 GB）。
// 上游 Virtualis 不计量流量：Levis 的流量配额只在本地累加，插件侧仅记录。
// 非该格式的 UNSPECIFIED 调用返回 false，ManageHost 仍报不支持的操作类型。
func parseTrafficTopUp(osField string) (int, bool) {
	raw := strings.TrimSpace(osField)
	if !strings.HasPrefix(raw, "traffic_gb=") {
		return 0, false
	}
	value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(raw, "traffic_gb=")))
	if err != nil || value < 1 || value > 10240 {
		return 0, false
	}
	return value, true
}

// power 执行电源操作并轮询确认（上游 PowerInstance 同步等待结果）。
func (p *virtualisPlugin) power(ctx context.Context, creds *credentials, hostID, action string) (*pb.ManageHostReply, error) {
	if err := p.apiPost(ctx, creds, "/instances/"+hostID+"/power", map[string]any{"action": action}, nil); err != nil {
		// Older Virtualis masters only know the soft action names. Preserve a
		// useful compatibility fallback for hard boot, which is equivalent there.
		if action == "hard_start" {
			if fallbackErr := p.apiPost(ctx, creds, "/instances/"+hostID+"/power", map[string]any{"action": "start"}, nil); fallbackErr == nil {
				return &pb.ManageHostReply{Success: true}, nil
			}
		}
		return &pb.ManageHostReply{Error: err.Error()}, nil
	}
	return &pb.ManageHostReply{Success: true}, nil
}

func (p *virtualisPlugin) powerWithImage(ctx context.Context, creds *credentials, hostID, action, imageID string) (*pb.ManageHostReply, error) {
	id, err := strconv.ParseUint(imageID, 10, 64)
	if err != nil || id == 0 {
		return &pb.ManageHostReply{Error: "镜像 ID 无效"}, nil
	}
	if err := p.apiPost(ctx, creds, "/instances/"+hostID+"/power", map[string]any{"action": action, "image_id": id}, nil); err != nil {
		return &pb.ManageHostReply{Error: err.Error()}, nil
	}
	return &pb.ManageHostReply{Success: true}, nil
}

func (p *virtualisPlugin) GetHost(ctx context.Context, req *pb.GetHostRequest) (*pb.GetHostReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	var instance v1Instance
	if err := p.apiGet(ctx, creds, "/instances/"+req.GetHostId(), &instance); err != nil {
		return &pb.GetHostReply{Error: err.Error()}, nil
	}
	name := instance.Name
	if name == "" {
		name = fmt.Sprintf("实例 #%s", req.GetHostId())
	}
	spec := fmt.Sprintf("%d 核 / %d MB / %d GB", instance.Spec.CPU, instance.Spec.MemoryMB, instance.Spec.DiskGB)
	ip := instance.ObservedIP
	if ip == "" {
		ip = instance.IP
	}
	sshHost, sshPort := ip, int32(22)
	if instance.Network.Mode == "nat" {
		sshHost, sshPort = "", 0
		if instance.Agent != nil {
			sshHost = instance.Agent.IP
		}
		for _, mapping := range instance.NATMappings {
			if mapping.Protocol == "tcp" && mapping.GuestPort == 22 {
				sshPort = int32(mapping.HostPort)
				break
			}
		}
	}
	resources := &pb.HostResources{
		Cpu: int32(instance.Spec.CPU), MemoryMb: int64(instance.Spec.MemoryMB),
		DiskGb: int64(instance.Spec.DiskGB), BandwidthMbps: int64(instance.Network.BandwidthMbps),
	}
	network := &pb.HostNetwork{
		Mode: instance.Network.Mode, Ipv4: ip, Mac: instance.Network.MAC,
		Gateway: instance.Network.Gateway, Dns: append([]string(nil), instance.Network.DNS...),
	}
	ssh := &pb.HostSSH{Host: sshHost, Port: sshPort, Username: "root", Password: instance.SSHPassword, Ready: instance.SSHReady}
	// 访问信息以 /access 为准：主控在未就绪时会先对账一次被控状态，
	// NAT 机器的连接地址（host:port）与 ready 只有这里才是实时事实。
	var access struct {
		Network *pb.HostNetwork `json:"network"`
		SSH     *pb.HostSSH     `json:"ssh"`
	}
	if err := p.apiGet(ctx, creds, "/instances/"+req.GetHostId()+"/access", &access); err == nil {
		if access.SSH != nil && access.SSH.Host != "" {
			ssh = access.SSH
		}
		if access.Network != nil && access.Network.Ipv4 != "" {
			ip = access.Network.Ipv4
			network.Ipv4 = ip
		}
	}
	return &pb.GetHostReply{
		Host: &pb.UpstreamHost{
			Id: req.GetHostId(), ProductName: fmt.Sprintf("%s（%s）", name, spec),
			Status: v1Status(instance.Status), Actions: []string{"boot", "shutdown", "reboot", "hard_boot", "hard_stop", "hard_restart", "reinstall"},
			Resources: resources, Network: network, Ssh: ssh,
			Cpu: int32(instance.Spec.CPU), MemoryMb: int64(instance.Spec.MemoryMB), DiskGb: int64(instance.Spec.DiskGB),
			BandwidthMbps: int64(instance.Network.BandwidthMbps), Ipv4: ip,
			SshHost: ssh.Host, SshPort: ssh.Port, SshUsername: ssh.Username, SshPassword: ssh.Password, SshReady: ssh.Ready,
		},
	}, nil
}

// ListHostOS 返回该实例所用驱动下的可用镜像（重装场景；当前 ManageHost
// 不支持重装，保留接口兼容）。
func (p *virtualisPlugin) GetHostMetrics(ctx context.Context, req *pb.GetHostMetricsRequest) (*pb.GetHostMetricsReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	var out struct {
		Metrics struct {
			CPUPercent     float64 `json:"cpu_percent"`
			MemoryUsedMB   int64   `json:"memory_used_mb"`
			MemoryTotalMB  int64   `json:"memory_total_mb"`
			NetworkRxBytes uint64  `json:"network_rx_bytes"`
			NetworkTxBytes uint64  `json:"network_tx_bytes"`
			BandwidthRxBps float64 `json:"bandwidth_rx_bps"`
			BandwidthTxBps float64 `json:"bandwidth_tx_bps"`
			CollectedAt    string  `json:"collected_at"`
		} `json:"metrics"`
	}
	if err := p.apiGet(ctx, creds, "/instances/"+req.GetHostId()+"/metrics", &out); err != nil {
		return &pb.GetHostMetricsReply{Error: err.Error()}, nil
	}
	return &pb.GetHostMetricsReply{Metrics: &pb.HostMetrics{
		CpuPercent: out.Metrics.CPUPercent, MemoryUsedMb: out.Metrics.MemoryUsedMB,
		MemoryTotalMb: out.Metrics.MemoryTotalMB, NetworkRxBytes: out.Metrics.NetworkRxBytes,
		NetworkTxBytes: out.Metrics.NetworkTxBytes, BandwidthRxBps: out.Metrics.BandwidthRxBps,
		BandwidthTxBps: out.Metrics.BandwidthTxBps, CollectedAt: out.Metrics.CollectedAt,
	}}, nil
}

func (p *virtualisPlugin) GetHostAccess(ctx context.Context, req *pb.GetHostAccessRequest) (*pb.GetHostAccessReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	var out struct {
		Network *pb.HostNetwork `json:"network"`
		SSH     *pb.HostSSH     `json:"ssh"`
	}
	if err := p.apiGet(ctx, creds, "/instances/"+req.GetHostId()+"/access", &out); err != nil {
		return &pb.GetHostAccessReply{Error: err.Error()}, nil
	}
	return &pb.GetHostAccessReply{Network: out.Network, Ssh: out.SSH}, nil
}

func (p *virtualisPlugin) GetHostVNC(ctx context.Context, req *pb.GetHostVNCRequest) (*pb.GetHostVNCReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	// 短票由主控签发（120 秒有效、一次性），ws 地址按接口地址推导：
	// 主程序拿到 ticket 后用自己的通道（同源代理）建连，不直接暴露给浏览器。
	var issued struct {
		Ticket    string `json:"ticket"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := p.apiPost(ctx, creds, "/instances/"+req.GetHostId()+"/vnc-ticket", nil, &issued); err != nil {
		return &pb.GetHostVNCReply{Vnc: &pb.HostVNC{Available: false, Message: err.Error()}}, nil
	}
	if issued.Ticket == "" {
		return &pb.GetHostVNCReply{Vnc: &pb.HostVNC{Available: false, Message: "上游未签发 VNC 短票"}}, nil
	}
	wsURL := vncWebSocketURL(creds.apiURL, req.GetHostId(), issued.Ticket)
	return &pb.GetHostVNCReply{Vnc: &pb.HostVNC{
		Available: true, WsUrl: wsURL, Ticket: issued.Ticket, ExpiresAt: issued.ExpiresAt,
	}}, nil
}

// vncWebSocketURL 把接口的 http(s) 地址换成对应 ws(s) 的短票通道。
func vncWebSocketURL(apiURL, hostID, ticket string) string {
	base := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	lower := strings.ToLower(base)
	switch {
	case strings.HasPrefix(lower, "https://"):
		base = "wss://" + base[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		base = "ws://" + base[len("http://"):]
	default:
		base = "ws://" + base
	}
	return base + "/api/instances/" + hostID + "/vnc/ws-ticket?ticket=" + url.QueryEscape(ticket)
}

func (p *virtualisPlugin) ListHostOS(ctx context.Context, req *pb.ListHostOSRequest) (*pb.ListHostOSReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	var instance v1Instance
	if err := p.apiGet(ctx, creds, "/instances/"+req.GetHostId(), &instance); err != nil {
		return &pb.ListHostOSReply{Error: err.Error()}, nil
	}
	return p.listOS(ctx, creds, instance.Driver)
}

// ListProductOS 返回购买时可选的系统镜像，按 options.driver 过滤。
func (p *virtualisPlugin) ListProductOS(ctx context.Context, req *pb.ListProductOSRequest) (*pb.ListHostOSReply, error) {
	creds, err := credsFromConfig(req.GetInterfaceConfig())
	if err != nil {
		return nil, err
	}
	driver := strings.ToLower(strings.TrimSpace(req.GetOptions()["driver"]))
	if driver != "qemu" && driver != "incus" {
		driver = "incus"
	}
	return p.listOS(ctx, creds, driver)
}

// listOS 拉取指定驱动的可用镜像列表。
func (p *virtualisPlugin) listOS(ctx context.Context, creds *credentials, driver string) (*pb.ListHostOSReply, error) {
	images, err := p.listImages(ctx, creds, driver)
	if err != nil {
		return &pb.ListHostOSReply{Error: err.Error()}, nil
	}
	out := make([]*pb.OSImage, 0, len(images))
	for _, image := range images {
		if image.Status != "" && image.Status != "available" {
			continue
		}
		name := image.DisplayName
		if name == "" {
			name = image.Name
		}
		group := image.Driver
		if group == "" || group == "auto" {
			group = driver
		}
		out = append(out, &pb.OSImage{
			Id:    strconv.FormatUint(uint64(image.ID), 10),
			Name:  name,
			Group: group,
		})
	}
	return &pb.ListHostOSReply{Os: out}, nil
}

// =============================================================================
// 未实现的能力方法
// =============================================================================

func (p *virtualisPlugin) SendMail(context.Context, *pb.SendMailRequest) (*pb.SendMailReply, error) {
	return nil, status.Error(codes.Unimplemented, "本插件不提供邮件发送能力")
}

func (p *virtualisPlugin) CreatePayment(context.Context, *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	return nil, status.Error(codes.Unimplemented, "本插件不提供支付能力")
}

func (p *virtualisPlugin) QueryPayment(context.Context, *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	return nil, status.Error(codes.Unimplemented, "本插件不提供支付能力")
}

func (p *virtualisPlugin) VerifyPaymentCallback(context.Context, *pb.VerifyPaymentCallbackRequest) (*pb.VerifyPaymentCallbackReply, error) {
	return nil, status.Error(codes.Unimplemented, "本插件不提供支付能力")
}
