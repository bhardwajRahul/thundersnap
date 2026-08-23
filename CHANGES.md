
## v0.03 (THUNDERstruck)

* thunderboot with infiniblock: support for running as the root process
  within a VM with a block device as backing for its btrfs. This will be
  useful on Apple's virtualization framework in particular. The components
  for running it will be outside this repo.
  See README.thunderboot-builder.md.

* Every binary now has a build version: `ts version` checks the client and
  daemon agree over the control socket, and `ts --version` (and --version on
  thundersnapd, vshd, dist, etc.) prints it offline.

* The MCP server grew conversation-scoped background bash jobs and various
  refinements. It now works great with Aperture's MCP proxy.

* Mesh mode: tsm and thundersnapd route who-has / download-snap fetches
  through tsnet. Previously mesh mode wasn't working in real life, just in
  tests.
  
* Various improvements to session management and clean startup/shutdown of
  sessions within a frame.

* `ts frame <snap:snap:snap>` can now accept any other frame or ref for any
  of the components; it pulls the latest version of that frame's exiting
  component.

* Various bugfixes, especially related to multi-architecture support.
  `ts download-docker` was incorrectly assuming amd64 on all architectures
  for example.


## v0.02 (SNAPception)

* thundersnap is self hosting! You can now run thundersnap inside
  thundersnap (and so on). This means it's a viable development platform.

* Vastly improved 'make e2e' tests and migrated more of them out of not_e2e/
  (but there's still more to go)
  
* `ts snap` now can index in the background, making it finish instantly. Use -q for
  this. `ts go` also indexes in the background so it feels faster.

* `ts go <user>@<frame>` lets you enter a frame as a particular user.
  Previously we didn't have the user@ syntax.

* Added a basic Thundersnap MCP server, host-side string replacement, correct
  MCP error reporting, and process-group cleanup for vshd sessions. You can
  plug this into your favourite AI harness and give it sandbox superpowers.

* Fixed many bugs in ssh, sftp, autorun.

* Added experimental Nix frame builders using Debian or Alpine as the base.
  Enter a thundersnap container, run ./scripts/nix/build-nix-debian.sh to
  get started.
  
* Every ts command now has a --help flag to make it more self-documenting.


## v0.01 (the unbearable lightness of SNAPPING)

* Supports basic commands: snap, frame, ref, autorun, go, undo, download-docker,
  download-snap

* Lots of bugs still! But you have to start somewhere.
