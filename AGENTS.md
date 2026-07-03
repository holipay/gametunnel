# Commands for opencode

## GitHub 提交流程（禁止直接 push main）

```bash
# 1. 切换到 main 并拉取最新代码，清理已删除的远程分支
git checkout main
git pull
git remote prune origin

# 2. 创建新分支
BRANCH="fix/$(git rev-parse --short HEAD)-description"
git checkout -b "$BRANCH"

# 3. 修改代码后提交
git add <files>
git commit -m "fix: description"

# 4. 推送分支到远程
git push -u origin "$BRANCH"

# 5. 创建 PR
gh pr create \
  --title "fix: description" \
  --body "## Changes\n\n- change 1\n- change 2" \
  --base main \
  --head "$BRANCH"

# 6. 切回 main（可选）
git checkout main
```

## 重要规则

- **PR 创建后不要自行合并**，等待用户指令合并或由其他人合并
- PR 合并后清理本地和远程分支
- 合并后切回 main 并 pull
