# Releasing

This fork publishes releases at `vicnoah/mygo-cloudmusic`. Windows builds are
unsigned; if the fork has no macOS certificate secrets, macOS builds are also
unsigned. Update manifests are signed when `MYGO_UPDATER_PRIVATE_KEY` is
available.

1. Commit pending work, bring the fork's `release` branch up to date, and run `go test ./internal/...`. The release branch is the only branch used for our published builds:

   ```bash
   git switch release
   git pull --rebase fork release
   ```

2. Bump `version` in `mygo.json`, and add a matching section at the top of `CHANGELOG.md` saying, in Chinese, what changed for people using the app since the last release. Commit both in a commit titled just the version, e.g. `0.2.4`. MyGo's updater shows that section in the app's update window, and the release page takes it as its notes; `mygo build` fails without one.
3. Push `release` first, and tag only once that push has gone through. Pushing both at once still pushes the tag when the branch is rejected, and the tag starts a release build of a commit that isn't on the fork:

   ```bash
   git push fork release
   git tag v0.2.4
   git push fork v0.2.4
   ```

   If `release` is rejected, pull again, add what came in to the changelog, amend the version commit, test, and push `release` again before tagging. If a tag went up on the wrong commit anyway, cancel its run, then delete the tag and the draft it made before tagging again:

   ```bash
   git push fork :refs/tags/v0.2.4
   gh release delete v0.2.4 --repo vicnoah/mygo-cloudmusic --yes
   ```

4. `.github/workflows/release.yml` builds macOS, Windows and Linux and uploads them to a draft release. Wait for every job to pass:

   ```bash
   gh run watch "$(gh run list --repo vicnoah/mygo-cloudmusic --workflow release.yml -L1 --json databaseId -q '.[0].databaseId')" --repo vicnoah/mygo-cloudmusic --exit-status
   ```

   If a job fails, don't publish; fix it first.

5. Publish the draft and mark it latest:

   ```bash
   gh release edit v0.2.4 --repo vicnoah/mygo-cloudmusic --draft=false --latest
   ```

6. Check the update feed shows the new version:

   ```bash
   curl -sL https://github.com/vicnoah/mygo-cloudmusic/releases/latest/download/update-darwin-arm64.json
   ```

7. Deploy the website. It reads the latest release when it's prerendered, so it has to be rebuilt after publishing:

   ```bash
   cd website && bun run deploy
   ```

   Then check https://mygo-cloudmusic.egoist.workers.dev/download/ shows the new version.

## The speech recognition libraries

Podcast captions run sherpa-onnx, whose libraries the app downloads with the model from the `sherpa-onnx-<version>` release, not from the app's own. They only change with sherpa-onnx's version, in `scripts/sherpa`. Then pack them again, signing the macOS ones with the app's Developer ID (the app's hardened runtime loads no others), commit the `internal/speech/natives.go` it writes, and upload them as a prerelease, which never becomes the latest that updates look for:

```bash
go run ./scripts/sherpa -sign "Developer ID Application: xinyu zeng (GJE9R5VE87)"
gh release create sherpa-onnx-2.0.1 --prerelease --latest=false build/sherpa/*
```

## macOS libmpv 完整资源包

macOS 不再发布只有主库的 `.dylib.gz`：主库及全部非系统 Mach-O 依赖必须放在同一个平铺 `.tar.gz` 中。独立资源 tag 使用 `dustPyrotechnic/mygo-cloudmusic` 的 `libmpv-macos-v1`，**不是应用版本，也不是官方 mpv 版本**。不要覆盖旧资源、修改 Windows/Linux 的下载地址或把资源 release 标为 latest。

### 固定来源与许可证

采用 [AIOStreams/libs 的 `20261117-0a3dc26` release](https://github.com/AIOStreams/libs/releases/tag/20261117-0a3dc26)，构建仓库提交为 `0a3dc263d808bdf01d5f25278357ba0909816bdf`。这是第三方完整播放器库，不是 mpv 官方二进制；仅更换解码资源，不启用视频输出或引入其应用运行配置。

| 架构 | 原始资产（同一 release 的下载目录） | SHA256 |
| --- | --- | --- |
| arm64 | `libs-macos-arm64.zip` | `2f5994352805a18550a5ddf86bdd98fc126a31766c643866052562a07a6ad83b` |
| amd64 | `libs-macos-x86_64.zip` | `15bdffbfed2f9f42795c46d52602f16302c235c885262c83dfc6bd6a00426343` |

固定源码：

- [mpv：`aeb726b441fd36b1ad4c2d34699e9e2ac0bb8a5f`](https://github.com/AIOStreams/mpv/tree/aeb726b441fd36b1ad4c2d34699e9e2ac0bb8a5f)。
- [FFmpeg：`9f0cc7afcc16f07bbc6d618367cbd14bf54e97b9`](https://github.com/AIOStreams/FFmpeg/tree/9f0cc7afcc16f07bbc6d618367cbd14bf54e97b9)。
- [libass：`77731e1160d4d1327780b384a169df7857ee17e4`](https://github.com/AIOStreams/libass/tree/77731e1160d4d1327780b384a169df7857ee17e4)。
- [其余组件版本、构建参数和获取脚本](https://github.com/AIOStreams/libs/tree/0a3dc263d808bdf01d5f25278357ba0909816bdf)；`sources.sh` 固定各源码版本/提交，`scripts/macos.sh` 指定 macOS 11.0 deployment target。发布前仍须检查实际所有 dylib 的最低系统版本及两个架构的加载/播放，不用 README 代替二进制验证。

上游脚本的 MIT 许可证**不等于二进制许可证**：上游说明此 libmpv 整体为 GPL-3.0，mpv/FFmpeg 包含 MPVKit 的 GPL-3.0-only 补丁。必须保留整个 `licenses/`（包含静态链接组件、MPVKit 归属和 `index.json`），并提供对应源码及构建脚本、补丁和子模块的获取方式；按适用 GPL 分发条件提供完整对应源码，不能只附 mpv 的一份许可证。以下打包会将全部嵌套许可证按原相对路径标题汇总到 `licenses-NOTICES.txt`，原文及来源索引不丢失。本说明不替代许可证审查。

### 下载、验证和打包

在仓库根目录用 Bash 执行；需要 macOS、Go、Xcode Command Line Tools。先校验压缩文件，再解压：

```bash
set -euo pipefail
source_dir=build/mpv-aiostreams-source
upstream=https://github.com/AIOStreams/libs/releases/download/20261117-0a3dc26
mkdir -p "$source_dir/arm64" "$source_dir/amd64"
curl -fL "$upstream/libs-macos-arm64.zip" -o "$source_dir/arm64.zip"
curl -fL "$upstream/libs-macos-x86_64.zip" -o "$source_dir/amd64.zip"
printf '%s  %s\n' \
  2f5994352805a18550a5ddf86bdd98fc126a31766c643866052562a07a6ad83b "$source_dir/arm64.zip" \
  15bdffbfed2f9f42795c46d52602f16302c235c885262c83dfc6bd6a00426343 "$source_dir/amd64.zip" \
  | shasum -a 256 -c -
unzip -q "$source_dir/arm64.zip" -d "$source_dir/arm64"
unzip -q "$source_dir/amd64.zip" -d "$source_dir/amd64"
pin=https://raw.githubusercontent.com/AIOStreams/libs/0a3dc263d808bdf01d5f25278357ba0909816bdf
curl -fL "$pin/README.md" -o "$source_dir/SOURCE-README.txt"
curl -fL "$pin/sources.sh" -o "$source_dir/sources.sh"
curl -fL "$pin/scripts/fetch.sh" -o "$source_dir/fetch.sh"
curl -fL "$pin/scripts/macos.sh" -o "$source_dir/macos.sh"

download_base=https://github.com/dustPyrotechnic/mygo-cloudmusic/releases/download/libmpv-macos-v1
for arch in arm64 amd64; do
  go run ./scripts/mpv \
    -platform "darwin-$arch" -input "$source_dir/$arch" -out build/mpv \
    -sign - -url-base "$download_base" \
    -license "$source_dir/$arch/licenses" \
    -license "$source_dir/SOURCE-README.txt" -license "$source_dir/sources.sh" \
    -license "$source_dir/fetch.sh" -license "$source_dir/macos.sh"
done
```

`-input` 也可指向具体主库、完整库目录或 `.app`；`.app` 从 `Contents/Frameworks` 收集依赖，并解析原 `@executable_path`。工具递归解析依赖、检查目标架构，拒绝缺库、歧义及平铺文件名冲突，复制而不修改输入；非系统引用和 install ID 改为 `@loader_path/<文件名>`，然后逐库重签名和校验。已正确重定位的引用不重复修改。Universal 库会抽取指定架构。签名后按固定排序、时间、权限与 USTAR 常规成员格式归档，不包含目录、符号链接、PAX 或硬链接。

输出仅为 `build/mpv/darwin-{arm64,amd64}-libmpv.tar.gz` 和 `darwin-{arm64,amd64}.json`，**不会改写 `natives.go`**。JSON 以平台为键，字段与 `nativeFile` 对应；`SHA256` 是签名后主库哈希，`Size` 是所有常规成员解压大小之和（含许可证），`GzSHA256/GzSize` 是整个归档的哈希和长度，`Bundle=true`，`URL` 是最终资源地址。将两个 JSON 对应值填入 `internal/player/mpv/natives.go` 的 darwin 项，保留其他平台值和旧默认下载地址。旧 `LibVersion=0.38.0` 不能作为新 macOS 二进制的真实版本描述。

非 macOS 保留同一命令的单库 gzip，例如 `go run ./scripts/mpv -platform linux-amd64 -input /path/to/libmpv.so.2 -out build/mpv`；它不附加 macOS URL，也不声称收集 Linux/Windows 依赖。

### 签名、验证和独立发布

- 默认 `-sign -` 是 ad-hoc，只适用于允许这种库的开发/未 hardened-runtime 应用。正式启用 library validation 的应用必须用**与应用同团队**的 Developer ID 签名每个库：将 `-sign -` 替换为 `-sign "$DEVELOPER_ID_APPLICATION"`，并按发布策略使用 `-timestamp`。不能靠本机 ad-hoc 加载成功宣称正式签名环境可用。
- 默认关闭签名时间戳，固定输入与工具链可生成确定性的 ad-hoc 包；正式安全时间戳会改变签名和归档哈希，必须在最终签名后重新生成 JSON，不能沿用开发包元数据。
- 集成时运行 `go test ./scripts/mpv ./internal/player/mpv`。打包测试覆盖递归闭包、缺库/冲突/架构错误、确定性 USTAR、完整许可证和真实 Mach-O 重写/签名。另在 arm64 和 Intel（或 Rosetta）验证实际资源经公共下载→安装→加载→音频播放路径，检查所有库 `otool -L`、最低系统版本及 `codesign --verify --strict`；没有 Homebrew 搜索路径也必须可用。
- 所有检查通过、用户明确授权后才发布以下**预发布资源**，不要触发应用 `v*` 版本发布流程。Release 说明必须附上述源码/构建/许可证来源与对应源码获取方式；资源发布后再验证两个 URL。不要在预发布上传前让正式应用依赖尚不存在的 URL。

```bash
gh release create libmpv-macos-v1 \
  --repo dustPyrotechnic/mygo-cloudmusic --prerelease --latest=false \
  --title "macOS libmpv 完整资源 v1（AIOStreams 20261117-0a3dc26）" \
  --notes "第三方完整库来源：https://github.com/AIOStreams/libs/releases/tag/20261117-0a3dc26 。构建脚本和所有依赖源码版本/获取方式：https://github.com/AIOStreams/libs/tree/0a3dc263d808bdf01d5f25278357ba0909816bdf （sources.sh、scripts/fetch.sh）。mpv 源码：https://github.com/AIOStreams/mpv/tree/aeb726b441fd36b1ad4c2d34699e9e2ac0bb8a5f 。FFmpeg 源码：https://github.com/AIOStreams/FFmpeg/tree/9f0cc7afcc16f07bbc6d618367cbd14bf54e97b9 。整体 libmpv 为 GPL-3.0，包含 MPVKit GPL-3.0-only 补丁；完整组件许可证与索引随包附带。此 tag 不是官方 mpv 版本。" \
  build/mpv/darwin-arm64-libmpv.tar.gz build/mpv/darwin-amd64-libmpv.tar.gz \
  build/mpv/darwin-arm64.json build/mpv/darwin-amd64.json
```
