# Claude Code Project Notes

- Always run "make test" after making changes.
- To build: "make binaries" (puts them in bin/) or "make ts" (just the ts binary)
- This project workspace may be using the 'jj' tool instead of git. Always
  check for jj first. If using jj, when making a fix, always `jj describe` it
  when done and then `jj new` so you don't accidentally mix changes
  together. Be careful that `jj log` shows not just this branch, but other
  branches mixed into the hierarchy!
