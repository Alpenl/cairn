// Package arch 只承载架构约束测试，不含生产代码。规则通过 go list 读取真实导入
// 图，确保低层不导入高层；零层叶子包（如 dto）可被任意上层使用。
//
// 四条规则各自堵一类漏洞，都是被真实漏检倒逼出来的：
//   - TestNoLowerLayerImportsUpperLayer   方向倒置
//   - TestAllInternalPackagesAreLayered   漏列包 = 免检
//   - TestEverySkipLayerPairIsRegistered  漏列跳层包对 = 免检
//   - TestSkipLayerImportsDoNotGrow       已登记包对的文件清单漂移
package arch
