# PR CI priority threat model

The `pull_request` CI workflows run workflow definitions from the PR merge commit. They must not perform Actions API cancellations or dispatch recovery with a write token.

`.github/workflows/pr-ci-priority.yml` is the trusted control plane for those operations. Its workflow definition and checked-out helper come from the base branch. It reads the pull request number as data, validates it as numeric, and only cancels runs from the fixed CI, performance, and browser workflow allowlist. It does not check out or execute pull request code or artifacts. Its token is limited to Actions write, contents read, and commit-status write.

For each main CI attempt, the trusted `workflow_run` handler marks a unique commit status pending, cancels active PR checks, and saves the exact cancelled run targets as an artifact keyed to that main attempt. The main CI priority job waits for that status before any fan-out; it fails closed if cancellation or target persistence fails. After the main aggregate completes, the trusted dispatcher downloads that attempt's artifact and passes the recorded targets to recovery, including targets created before main started. `pull_request_target` and PR `workflow_run` events cover new PR activity while main is active. The recovery workflow checks that the PR is still open at the cancelled run's head before rerunning it.

PR build and test jobs continue to run under `pull_request`; they do not receive repository secrets through this control plane. Their checkout credentials are not persisted in the Actions write cache jobs, and the admin-browser job no longer gets Actions write just to delete an invalid cache entry. Cache writes remain scoped to the PR merge ref. Any future authenticated API operation must stay in the trusted control plane, not in a `pull_request` workflow.

This boundary covers the priority and recovery API operations. Any change to a PR workflow's token permissions or to a job that receives an explicit token remains a privileged workflow change and needs review; this file does not claim to make arbitrary same-repository workflow edits safe.
