# plugin_levis-virtualis

Levis 的 Virtualis 对接插件：让下游 Levis 销售面板对接上游 [Virtualis](https://github.com/SakuraOpenSource/virtualis)
弹性云主机系统，销售容器（Incus）与虚拟机（QEMU）实例。

## 与其他开通插件的差别

- **无插件全局配置**。插件本体不保存任何凭据；在 Levis 后台 **接口管理**
  可以为同一个模块配置多个接口（不同的 Virtualis 站点地址 + 站点 API Key），
  配置经每个 gRPC 调用的 `interface_config` 按请求透传。
- **商品在本地创建**。上游没有「产品」概念：新建商品时选择接口，
  再创建两种配置之一 ——
  - **弹性云**：设定 CPU / 内存 / 硬盘 / 带宽 / 流量（0 即不限，可用 TB/GB 单位）
    的最小值与最大值，用户购买时在区间内自选；
  - **管理员固定配置**：各项规格固定，购买页仅展示。
- **购买页选择系统镜像**。按商品驱动自动过滤：Incus 驱动只能装 Incus 镜像，
  QEMU 驱动只能装 QEMU 镜像。

## 依赖上游

Virtualis 主控需开放 `/api/v1` 机器 API（`X-Virtualis-Api-Key` 鉴权，
支持自动选择在线节点创建实例）。密钥在 Virtualis 后台「安全 → API 密钥」生成。

## 构建

```bash
./build.sh   # 产物：dist/virtualis-<os>-<arch>.zip
```

> go.mod 通过 `replace ../levis` 引用插件 SDK，构建需同级目录存在 levis 仓库。

将对应平台 ZIP 在 Levis 后台「插件」页上传安装，然后：

1. 接口管理 → 新增接口（模块选「Virtualis 对接」，填地址与密钥，测试连通）
2. 商品管理 → 新建商品（接口 + 驱动 + 弹性/固定配置）
3. 用户从商品卡「立即购买」进入购买页，选配下单，支付后自动开通。

## 已购服务管理

- 开机 / 关机 / 重启（映射上游电源操作）
- 删除服务会同步删除上游实例
- 续费仅顺延本地周期（Virtualis 不做上游计费）
- 重装系统暂不支持，请在上游操作
