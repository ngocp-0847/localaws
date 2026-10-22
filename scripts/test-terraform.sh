#!/usr/bin/env bash
# The Terraform AWS provider against localaws, for real (make test-terraform):
# examples/terraform is applied, must plan clean, is updated (rev=2), must plan clean
# again, and is destroyed. Needs terraform (or TERRAFORM=tofu) and network access for
# `terraform init` the first time (TF_PLUGIN_CACHE_DIR is honoured).
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tf="${TERRAFORM:-terraform}"
free_port() { # a port nothing listens on
  local p
  for _ in $(seq 50); do
    p=$((20000 + RANDOM % 20000))
    (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null || { echo "$p"; return; }
  done
  return 1
}
port="${PORT:-$(free_port)}"
work="$(mktemp -d)"
trap 'kill $pid 2>/dev/null || true; wait $pid 2>/dev/null || true; rm -rf "$work"' EXIT

(cd "$root" && CGO_ENABLED=0 go build -o "$work/localaws" .)
"$work/localaws" -port "$port" -data "$work/data" -runner noop -region ap-northeast-1 >"$work/localaws.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -fsS -o /dev/null "http://127.0.0.1:$port/_localaws/health" 2>/dev/null && break; sleep 0.1; done
kill -0 "$pid" 2>/dev/null || { cat "$work/localaws.log"; echo "localaws did not start on :$port"; exit 1; }
echo "localaws on :$port (log $work/localaws.log)"

cp -r "$root/examples/terraform" "$work/stack"
rm -rf "$work/stack/.terraform" "$work/stack"/terraform.tfstate*
cd "$work/stack"
v=(-var "localaws_endpoint=http://localhost:$port" -input=false -no-color)
step() { local t0=$SECONDS; echo "── $*"; "$@" >"$work/step.log" 2>&1 || { local rc=$?; tail -40 "$work/step.log"; return $rc; }; echo "   ok ($((SECONDS - t0))s)"; }
clean() { # plan must report no changes
  echo "── $tf plan $* (expect no changes)"
  local rc=0; "$tf" plan -detailed-exitcode "${v[@]}" "$@" >"$work/plan.log" 2>&1 || rc=$?
  if [[ $rc != 0 ]]; then grep -E '^  #|^ +[~+-] ' "$work/plan.log" | head -60 || tail -40 "$work/plan.log"; echo "   FAIL: plan rc=$rc"; return 1; fi
  echo "   ok"
}
step "$tf" init -input=false -no-color
step "$tf" apply -auto-approve "${v[@]}"
clean
step "$tf" apply -auto-approve "${v[@]}" -var rev=2
clean -var rev=2
step "$tf" destroy -auto-approve "${v[@]}" -var rev=2
echo "terraform: apply → plan → update → plan → destroy all passed"
