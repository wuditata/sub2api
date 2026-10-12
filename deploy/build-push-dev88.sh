#!/usr/bin/env bash
# 构建「上游版本 + 私有第四位」的镜像并推送到阿里云 ACR dev88 命名空间。
#
# 版本规则：上游三段版本（来自 backend/cmd/server/VERSION，随合并上游自动更新）
# + 第四位私有构建号（全局单调递增，持久化在 deploy/PRIVATE_REV）。
# 例：0.2.15.7 → 合并上游 0.2.16 后第一次构建 → 0.2.16.8
#
# 构建号语义：每个「新代码状态」分配一个号；同一 commit 重复构建复用同一个号。
# 判断代码是否变化时排除 PRIVATE_REV / VERSION 两个记账文件，避免记账 commit
# 本身触发递增。merge/revert/cherry-pick 不改变树内容时同样不递增。
#
# 内置更新检查器只比较前三段，所以 0.2.16.8 对比上游 0.2.16 显示"已是最新"，
# 上游发 0.2.17 时正常提示可更新。
#
# 用法：
#   ./deploy/build-push-dev88.sh                  # 正常构建并推送
#   ./deploy/build-push-dev88.sh -r 3             # 强制指定第四位（不入库，应急用）
#   ./deploy/build-push-dev88.sh --dry-run        # 只打印将执行的命令，不递增计数
#
# 前置：已 docker login registry.cn-hangzhou.aliyuncs.com。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
REGISTRY="registry.cn-hangzhou.aliyuncs.com"
NAMESPACE="dev88"
IMAGE="${REGISTRY}/${NAMESPACE}/sub2api"
REV_FILE="${SCRIPT_DIR}/PRIVATE_REV"

DRY_RUN=false
FORCE_REV=""
while [ $# -gt 0 ]; do
    case "$1" in
        -r|--rev) FORCE_REV="$2"; shift 2 ;;
        --dry-run) DRY_RUN=true; shift ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

# ── 上游三段版本 ──
BASE_VERSION="$(tr -d '\r\n' < "${REPO_ROOT}/backend/cmd/server/VERSION")"
if [ -z "${BASE_VERSION}" ]; then
    echo "ERROR: backend/cmd/server/VERSION 为空" >&2
    exit 1
fi

# ── 构建号判定 ──
# 语义：HEAD 本身是记账 commit（由本脚本创建）→ 说明当前代码状态已分配过号，复用；
# 否则视为新代码状态，递增并创建记账 commit。记账 message 记录分配时的代码 commit
# （记账的父 commit），因此「第 N 版 ↔ 哪个代码」永久可追溯。
BUMP_RE='^chore\(deploy\): bump private build rev to ([0-9]+) \(commit:([0-9a-f]+)\)$'
HEAD_SUBJECT="$(git -C "${REPO_ROOT}" log -1 --format=%s)"
CURRENT_REV="$(git -C "${REPO_ROOT}" show HEAD:deploy/PRIVATE_REV 2>/dev/null || echo 0)"

if [ -n "${FORCE_REV}" ]; then
    REV="${FORCE_REV}"
    echo "==> 使用手工指定构建号: ${REV}（不更新 ${REV_FILE}）"
elif [[ "${HEAD_SUBJECT}" =~ ${BUMP_RE} ]]; then
    REV="${CURRENT_REV}"
    echo "==> HEAD 已分配过构建号，复用: ${REV}"
else
    REV=$(( CURRENT_REV + 1 ))
    CODE_COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short HEAD)"
    if [ "${DRY_RUN}" = false ]; then
        printf '%s\n' "${REV}" > "${REV_FILE}"
        git -C "${REPO_ROOT}" add "${REV_FILE}"
        git -C "${REPO_ROOT}" commit -q \
            -m "chore(deploy): bump private build rev to ${REV} (commit:${CODE_COMMIT})" \
            --no-verify
        echo "==> 构建号已递增并提交: ${REV} (deploy/PRIVATE_REV)"
    else
        echo "==> [dry-run] 将递增构建号 → ${REV} 并提交"
    fi
fi

VERSION="${BASE_VERSION}.${REV}"
COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short HEAD)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "==> 版本: ${VERSION}  (上游 ${BASE_VERSION} + 私有构建 ${REV}, commit ${COMMIT})"
echo "==> 镜像: ${IMAGE}:${VERSION} 与 ${IMAGE}:latest"

run() {
    echo "\$ $*"
    if [ "${DRY_RUN}" = false ]; then "$@"; fi
}

run docker build \
    --build-arg VERSION="${VERSION}" \
    --build-arg COMMIT="${COMMIT}" \
    --build-arg DATE="${DATE}" \
    --build-arg GOPROXY=https://goproxy.cn,direct \
    --build-arg GOSUMDB=sum.golang.google.cn \
    -t "${IMAGE}:${VERSION}" \
    -t "${IMAGE}:latest" \
    -f "${REPO_ROOT}/Dockerfile" \
    "${REPO_ROOT}"

run docker push "${IMAGE}:${VERSION}"
run docker push "${IMAGE}:latest"

echo ""
echo "完成。CCE 升级方式（任选其一）："
echo "  1) 固定 tag（推荐，可回滚）：无状态 → sub2api → 编辑YAML → 镜像改为 ${IMAGE}:${VERSION}"
echo "  2) 继续用 latest：无状态 → sub2api → 重新部署（会重新拉取 latest）"
echo ""
echo "别忘了推送记账 commit：git push origin main"
