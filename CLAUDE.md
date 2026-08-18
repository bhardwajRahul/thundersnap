Oh yes, this project is very AI-coded, with all the pros and cons that come
with it. The bad news is the quality is currently variable; the good news is
that this project would not have been possible at all without it, because
it's too many moving parts at once to be justifiable as a human-driven
experiment. Over time we need to improve the quality of all the components.

# Claude Code Project Notes

- Always run "make test" and then "make e2e" and then "make not_e2e" after making changes.
  "make test" also enforces gofmt; run "gofmt -w ." to fix formatting before committing.
- To build: "make binaries" (puts them in bin/) or "make ts" (just the ts binary)
- e2e tests MUST NEVER SKIP. "make e2e" must either fully pass every validation
  or fail. Never add t.Skip/t.Skipf/t.SkipNow to e2e tests; if a precondition
  (root, btrfs, VM deps) is missing, that is a misconfigured environment and the
  test must fail (t.Fatal), not skip. The e2e package is built with the "e2e"
  build tag so it is excluded from "make test" and only run via "make e2e".
- e2e tests run simplest-to-hardest in tiers (see e2e/main_test.go TestMain); if
  an early tier fails, later tiers are skipped so you debug the root cause first.
- To run a single e2e test, use E2E_ARGS with -test.run and log to a file:
  `make e2e E2E_ARGS="-test.run=TestSSHContainerBasic" 2>&1 | tee e2e.log`
  Always log output to a file (e2e tests are verbose). Tests typically complete
  in ~30s; use a 1-2 minute timeout when waiting.
- e2e test prerequisites (install if missing — tests will t.Fatal, not skip):
  - `busybox-static` (NOT `busybox` — the dynamic one can't exec inside
    nil:nil:nil frames which have no /lib64; `apt-get install busybox-static`)
  - `virtiofsd` and `passt` for VM tests (`apt-get install virtiofsd passt`)
  - `/dev/kvm` for VM tests (check for this! you almost certainly have it!)
  - Generated VM binaries (`cloud-hypervisor`, `vmlinux`) in `vm/` or set
    `THUNDERSNAP_VM_DIR`; on a fresh x86-64 Linux checkout run
    `make thunderboot-vm-artifacts` (see `README.thunderboot-builder.md`)
- This project workspace may be using the 'jj' tool instead of git. Always
  check for jj first.
- Commit messages: first line in the form "dir(s): what changed". Then 
  1-2 paragraphs about what/why. Try to keep line length <=72.
- Before assuming you can't run a command for lack of privileges, try `sudo`
  first. This environment has passwordless sudo.
