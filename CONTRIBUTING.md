# 参与贡献

## 开发环境

- macOS 14+
- Go 1.24+
- Python 3 + PyYAML
- mihomo（`brew install mihomo`），用于集成测试与本地运行

## 提交前检查

```bash
gofmt -l .            # 必须无输出
go vet ./...
go test ./...
python3 test_gen_config.py                   # 改动生成器时执行
python3 gui/test_labels.py                   # 改动 GUI 时执行
cd gui && ./build.sh /tmp/clashhub-check     # 改动 GUI 时执行
```

## 约定

- Go 代码保持 `gofmt` 格式；新增逻辑优先写单元测试，探测/选路相关改动应覆盖失败与不确定两种结果。
- GUI 文案使用中文，信息层级保持紧凑；新增设置项要有默认值，旧配置必须能无损加载。
- 不提交任何订阅地址、节点、token、密钥、真实域名或本机私有路径。
- 不修改 FlClash 的数据库或配置文件；所有聚合状态留在 ClashHub 自己的目录。
- 提交信息用一句话说明「改了什么、为什么」，不要只写「update」。

## 报告问题

请附上：macOS 版本、ClashHub 版本（`./clashhub status`）、复现步骤、`~/Library/Logs/ClashHub.log` 中相关片段（先确认没有订阅地址或 token）。安全问题请按 [SECURITY.md](SECURITY.md) 私下报告。
