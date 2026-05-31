# cloudraid 真实网盘基准测试报告

测试时间：2026-05-31

## 测试目标

对比同一个 4K HDR WebM 文件在以下两种路径下的上传与下载表现：

1. 直接上传/下载到 AList 的百度网盘挂载 `/baidu`
2. 通过 cloudraid 上传/下载，底层分片写入 `/baidu` 与 `/ali`

## 测试文件

- 本地路径：`/Users/huangsheng/Movies/Real-4K-HDR-60fps-LG-Jazz-HDR-UHD.webm`
- 文件大小：`485,597,827` bytes（约 463 MiB）
- 源文件 SHA-256：`77fc17596eff71cee4f57ecc89c24fbe8939bf008962e99b5bd93f1dcc0f5476`

## 测试环境

- AList 地址：`http://127.0.0.1:5244`
- cloudraid WebDAV：`0.0.0.0:5260`
- cloudraid 配置：`cloudraid/data/config.yaml`
- cloudraid 分片大小：`1,048,576` bytes（1 MiB）
- cloudraid 写并发：`2`
- cloudraid 读并发：`2`
- cloudraid 底层挂载：
  - `/baidu`（BaiduNetdisk）
  - `/ali`（AliyundriveOpen）

## 测试前清理

测试前已执行以下清理，避免历史文件和本地缓存影响结果：

- 删除 cloudraid 元数据库中的全部逻辑文件记录
- 通过 cloudraid 删除对应远端分片
- 清空 `cloudraid/data/cache`
- 下载测试前再次清空 `cloudraid/data/cache`，避免命中 write-through 本地缓存

清理后确认：

- `files` 表记录数：`0`
- `blocks` 表记录数：`0`
- cache 文件数：`0`

## 测试结果

| 项目 | 数据量 | 耗时 | 平均速度 | 说明 |
|---|---:|---:|---:|---|
| AList 百度盘上传 | `485,597,827` bytes | `82.00 s` | `5.65 MiB/s` | 完整上传 |
| cloudraid 上传 | `485,597,827` bytes | 约 `187 s` | `2.48 MiB/s` | 根据 AList 日志中 464 个分片 PUT 的首尾时间估算 |
| AList 百度盘下载采样 | `2,506,752` bytes | `30.13 s` | `0.079 MiB/s` | 30 秒超时采样，未等待完整下载 |
| cloudraid 下载采样 | `9,437,184` bytes | `60 s` | `0.15 MiB/s` | 清空 cache 后采样，未等待完整下载 |

## cloudraid 上传后的分片情况

cloudraid 上传路径：

- 逻辑路径：`/benchmark/Real-4K-HDR-60fps-LG-Jazz-HDR-UHD.webm`
- 文件 ID：`1248fd6e2a7fe644`
- 总分片数：`464`
- 元数据记录总大小：`485,597,827` bytes

分片分布：

| mount | 分片数 | 字节数 |
|---|---:|---:|
| `/ali` | `232` | `242,328,195` |
| `/baidu` | `232` | `243,269,632` |

## 下载采样校验

下载采样没有等待完整文件下载完成，原因是 AList 百度盘直连下载速度较慢。采样文件已按源文件前缀校验，确认不是错误页或重定向页面。

| 采样文件 | 字节数 | 是否匹配源文件前缀 |
|---|---:|---|
| AList 百度盘采样 | `2,506,752` | 是 |
| cloudraid 采样 | `9,437,184` | 是 |

## 观察结论

1. 在本次真实网盘测试中，cloudraid 下载采样速度约为 AList 百度盘单盘下载的 `1.9x`。
2. cloudraid 上传速度低于直接上传到 AList 百度盘，约为单盘上传的 `44%`。
3. cloudraid 下载前已经清空本地 cache，因此下载结果反映的是从远端网盘拉取分片的表现，而不是本地缓存命中。
4. cloudraid 上传较慢的可能原因包括：
   - 1 MiB 分片导致 464 次小文件上传，API 调用开销明显
   - 当前写并发为 2，无法充分摊薄单块上传延迟
   - `/ali` 与 `/baidu` 两个网盘的上传链路耗时不一致
   - AList 作为中间层处理大量小文件上传时存在额外开销

## 后续建议

1. 测试更大的 `stripe.block_size`，例如 4 MiB、8 MiB、16 MiB，减少小文件数量。
2. 分别测试 `write_workers` 为 2、4、8 时的上传耗时。
3. 为下载测试加入固定采样策略，例如固定 60 秒采样或固定 64 MiB 采样，避免单盘下载过慢导致测试不可控。
4. 如果目标是视频播放体验，建议单独测试 WebDAV 随机读、首屏打开耗时和 seek 延迟，而不只看完整顺序下载吞吐。
