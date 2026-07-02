# Commands for opencode

## GitHub 提交流程（禁止直接 push main）

```bash
# 1. 切换到 main 并拉取最新代码
git checkout main
git pull

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
