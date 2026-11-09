# CPA Image Parameter Injector

一个面向 CLIProxyAPI / EasyCLIProxyAPI 的原生动态库插件。它使用官方 C ABI 的 request_interceptor 能力，在上游请求发送前读取图片 API 参数，并将可表达的约束追加到 prompt。

## 兼容性

- 已验证核心版本：CLIProxyAPI 8.0.53。
- 构建环境：Go 1.26.8，Windows amd64 / Linux amd64，插件 ABI v1 / schema 10。
- GitHub 仓库：https://github.com/kjx372124896/CPA-Image-Parameter-Injector

## 当前支持

- 只处理 request_path 为 `/v1/images/generations` 和 `/v1/images/edits` 的请求。
- `/v1/images/generations` 支持 JSON 请求体；`/v1/images/edits` 支持 `multipart/form-data`。
- 图片编辑只修改文本 `prompt` 字段；原始 image、mask 等文件字段的文件名、MIME 头和文件内容保持不变，重建时沿用原 multipart boundary。
- size：写入目标像素尺寸，并从 1024x1536 推导 2:3 等比例。
- n：写入独立图片数量要求，明确禁止拼图、九宫格和联系表。
- aspect_ratio / ratio：优先使用显式比例；否则从 size 推导。
- quality、style、output_format、background：按配置转换为自然语言约束。
- 保留原请求中的 size、n 等字段，只修改 prompt。
- 通过固定标记块替换旧注入内容，重复经过拦截器不会累积参数块。
- 普通聊天、没有 prompt 的请求以及不在目标路径中的请求保持原样。

## 重要边界

Prompt 注入是兼容性补偿，不是底层 API 参数的等价替代。它能让模型看到尺寸、数量和画质要求，但不能保证模型严格输出指定像素，也不能保证不支持多图返回的上游真的返回 n 张独立文件。output_format 等编码参数尤其需要上游 API 本身支持。

## 构建

在 WSL Ubuntu 中安装 Go 1.26.x、gcc 和 gcc-mingw-w64-x86-64，然后按目标平台执行：

    ./build-windows.sh

Linux amd64 原生插件：

    ./build-linux.sh

正式 Release 使用 `manylinux2014_x86_64` 容器构建，并强制检查 GLIBC 2.17 基线，避免在较旧 Linux 云服务器上因 glibc 版本过高而无法加载。

Windows 输出：build/image-param-injector.dll。
Linux 输出：build/image-param-injector.so。

## 安装到 CPA

Windows：`plugins/windows/amd64/image-param-injector.dll`

Linux：`plugins/linux/amd64/image-param-injector.so`

macOS 暂未提供构建包。

在 config.yaml 的 plugins 节点中合并 config.example.yaml 里的 configs.image-param-injector 配置。不要覆盖现有 OAuth、API Key 或其他插件配置。

确认 plugins.enabled: true，单插件 enabled: true。之后通过 CPA 插件管理接口检查 registered 和 effective_enabled。

## 安装到 Windows CPA（手动）

把 DLL 放到 CPA 核心配置目录下的：

    plugins/windows/amd64/image-param-injector.dll

## 回滚

将 plugins.configs.image-param-injector.enabled 改为 false，或移走 image-param-injector.dll，再热重载/重启核心。删除本插件不会改动 OAuth 凭据。

