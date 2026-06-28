# 分支管理与 PR 规范

> 本文档记录了分支管理的最佳实践，基于 2026-06-28 会话中发现的问题总结。

## 背景

在本次会话中，仓库积累了 17 个未合并的远程分支，导致：
- 大量重复工作（多个分支做同样的 vendor 清理）
- 分支与 main 代码结构完全不兼容
- 合并时需要手动解决数百个冲突

## 问题根因

| 问题 | 影响 |
|------|------|
| 分支无保护 | 任何人都可以直接推送到 main，无需审查 |
| 分支未及时 rebase | 17个分支基于旧的共同祖先，与 main 已完全不兼容 |
| 重复工作 | 多个分支都做了 argon2→hkdf 迁移，各自独立 |
| 无 CI 检查 | 没有自动化验证分支是否与 main 兼容 |
| 无分支生命周期管理 | 过时分支无人清理 |

## 改进措施

### 1. GitHub Branch Protection（推荐优先配置）

**配置路径：** Settings → Branches → Branch protection rules → Add rule

**建议设置：**
- ✅ Require a pull request before merging
- ✅ Require approvals (1人以上)
- ✅ Require status checks to pass (CI)
- ✅ Require branches to be up to date before merging
- ❌ Allow force pushes (禁止)
- ❌ Allow deletions (禁止)

### 2. 分支生命周期规范

| 阶段 | 时间 | 操作 |
|------|------|------|
| 创建 | Day 0 | 从 main 创建功能分支 |
| 保持同步 | 每周 | rebase 到最新 main |
| 审查 | 随时 | 提交 PR，请求审查 |
| 合并 | 2周内 | 完成审查后合并 |
| 清理 | 合并后 | 删除远程分支 |
| 标记 | 30天 | 未合并分支标记为 stale |
| 关闭 | 60天 | 自动关闭过时分支 |

### 3. PR 流程规范

**PR 模板建议：**
```markdown
## 目的
<!-- 为什么要做这个改动？解决什么问题？ -->

## 改动
<!-- 具体改了什么？ -->

## 测试
<!-- 如何验证改动是正确的？ -->

## 关联 Issue
<!-- Closes #xxx -->
```

**原则：**
- 单一职责：一个 PR 只做一件事
- 保持小规模：改动不超过 500 行
- 及时响应：24小时内回复 review comment

### 4. CI/CD 增强

**建议添加的检查：**
- `golangci-lint` 静态分析
- `go vet` 和 `staticcheck`
- 测试覆盖率（目标 >50%）
- 依赖安全扫描（`govulncheck`）
- 编译检查（多平台）

### 5. 定期清理

**每月执行：**
```bash
# 列出超过30天未合并的分支
git branch -r --no-merged main --format='%(committerdate:relative) %(refname:short)' | sort

# 删除已合并的远程分支
git branch -r --merged main | grep -v HEAD | grep -v main | sed 's/origin\///' | xargs -I {} git push origin --delete {}
```

## 实施步骤

1. 在 GitHub 仓库设置中启用 Branch Protection Rules
2. 创建 `.github/CODEOWNERS` 文件指定审查者
3. 更新 CI 配置添加 PR 检查
4. 创建 GitHub Action 自动标记 stale 分支
5. 在 README 中添加分支管理规范

## 参考

- [GitHub Branch Protection](https://docs.github.com/en/repositories/configuring-a-repository's-repository-settings/configuring-branch-protection)
- [GitHub CODEOWNERS](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners)
- [Conventional Commits](https://www.conventionalcommits.org/)
