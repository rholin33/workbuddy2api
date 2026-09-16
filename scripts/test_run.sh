#!/bin/sh
# 在仓库内跑构建 + vet + 全量单测（用 docker build 的一次性 stage 复用 Dockerfile 工具链）。
set -e
cd /build
export GOPROXY=https://goproxy.cn,direct
echo "=== go build ./... ==="
go build ./...
echo "=== go vet ==="
go vet ./internal/... || true
echo "=== 失败用例明细 ==="
go test ./internal/... 2>&1 | grep -E "^(--- FAIL|    --- FAIL|ok  )" | head -40
echo "=== 上下文预算用例逐条 ==="
go test -v -run 'TestGuard|TestEstimateTokens|TestChatContextTooLong|TestClassifyContextTooLong' ./internal/server/ 2>&1 \
  | grep -E "^(=== RUN|--- (PASS|FAIL)|ok|FAIL|    context_budget_test)" | head -50

echo "（force rerun）"

echo "（force rerun 2）"

echo "（force rerun 3）"

echo "（force rerun 4）"
