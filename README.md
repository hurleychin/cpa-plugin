# CPA 插件仓库

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。当前提供 **WorkBuddy / CodeBuddy** OAuth Provider。

## 插件

| ID | 说明 | 源码 |
|---|---|---|
| `workbuddy` | Tencent CodeBuddy 企业版 OAuth、动态模型、executor、企业版用量配额（按月结算周期）、积分面板、可选积分调度 | [workbuddy/](workbuddy/) |

## 多架构 Release

插件独立版本发 Release（tag `workbuddy-v*`），产物为 CPA 插件商店标准格式：

```text
workbuddy_<version>_linux_amd64.zip      # zip 根目录: workbuddy.so
workbuddy_<version>_linux_arm64.zip
workbuddy_<version>_darwin_amd64.zip     # workbuddy.dylib
workbuddy_<version>_darwin_arm64.zip
workbuddy_<version>_windows_amd64.zip    # workbuddy.dll
workbuddy_<version>_windows_arm64.zip
workbuddy_<version>_freebsd_amd64.zip
checksums.txt
```

命名规则与官方一致：`ArchiveName(id, version, goos, goarch) = {id}_{version}_{goos}_{goarch}.zip`
（见 CLIProxyAPI `internal/pluginstore`）。

CI：push / PR 全量构建（只出 artifacts）；tag `workbuddy-v*` 或 dispatch 触发该插件独立版本的 Release。

## 安装（linux/amd64 示例）

```bash
# 从 Release 下载
unzip workbuddy_0.8.7_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp workbuddy.so /path/to/cliproxyapi/plugins/workbuddy.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp workbuddy.so plugins/linux/amd64/
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
```

## 远程更新（插件商店自定义源）

CPA 插件商店源添加：

```text
https://raw.githubusercontent.com/hurleychin/cpa-plugin/main/registry.json
```

然后在商店 UI 安装/更新 **workbuddy**。
