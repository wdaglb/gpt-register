// Package sentinel 收口项目当前使用的 OpenAI Sentinel 生成逻辑。
//
// Why: 之前通过本地 replace 依赖外部 openai-sentinel-go 模块，GitHub Actions
// 无法访问该绝对路径。把运行所需实现迁移到仓库内部后，构建、发布和二进制分发
// 都能基于单仓库完成，不再依赖开发机上的私有目录结构。
package sentinel
