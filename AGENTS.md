# Commands for opencode

## GitHub 提交流程（禁止直接 push main）

```bash
# 1. 在 main 上提交
git add <files> && git commit -m "message"

# 2. 创建分支并推送
BRANCH="fix/$(git rev-parse --short HEAD)-description"
git checkout -b "$BRANCH"
git push -u origin "$BRANCH"

# 3. 创建 PR
gh pr create \
  --title "fix: description" \
  --body "## Changes\n\n- change 1\n- change 2" \
  --base main \
  --head "$BRANCH"

# 4. 切回 main
git checkout main
```
