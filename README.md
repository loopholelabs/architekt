<img alt="Project icon" style="vertical-align: middle;" src="./docs/icon.svg" width="128" height="128" align="left">

# Drafter

Minimal VM primitive with live migration support.

<br/>

[![hydrun CI](https://github.com/loopholelabs/drafter/actions/workflows/hydrun.yaml/badge.svg)](https://github.com/loopholelabs/drafter/actions/workflows/hydrun.yaml)
![Go Version](https://img.shields.io/badge/go%20version-%3E=1.21-61CFDD.svg)
[![Go Reference](https://pkg.go.dev/badge/github.com/pojntfx/loopholelabs/drafter.svg)](https://pkg.go.dev/github.com/pojntfx/loopholelabs/drafter)

## Overview

Drafter is a fast and minimal VM manager with live migration support.

It enables you to ...

- **Snapshot, package, and distribute stateful VMs**: With an opinionated packaging format and simple developer tools (`drafter-snapshotter` and `drafter-runner`), managing, packaging, and distributing VMs becomes as straightforward as working with containers.
- **Run OCI images as VMs**: In addition to running almost any Linux distribution (Alpine Linux, Fedora, Debian, Ubuntu etc.), Drafter can also run OCI images as VMs without the overhead of a nested Docker daemon or full CRI implementation. It uses a dynamic disk configuration system, an optional custom Buildroot-based OS to start the OCI image, and a familiar Docker-like networking configuration with `drafter-forwarder`.
- **Easily live migrate VMs between heterogeneous nodes with no downtime**: Drafter leverages a [custom optimized Firecracker fork](https://github.com/loopholelabs/firecracker) and [patches to PVM](https://github.com/loopholelabs/linux-pvm-ci) to enable live migration of VMs between heterogeneous nodes, even across continents. With a [customizable hybrid pre- and post-copy strategy](https://pojntfx.github.io/networked-linux-memsync/main.pdf), migrations typically take below 100ms within the same datacenter and around 500ms for Europe ↔ North America migrations over the public internet, depending on the application.
- **Hook into suspend and resume lifecycle with agents**: Drafter uses a VSock- and [panrpc](https://github.com/pojntfx/panrpc)-based agent system to signal to guest applications before a suspend/resume event, allowing them to react accordingly.
- **Easily embed VMs inside your applications**: Drafter provides a powerful, context-aware [Go library](https://pkg.go.dev/github.com/pojntfx/loopholelabs/drafter) for all system components, including `drafter-nat` for guest-to-host networking, `drafter-forwarder` for local port-forwarding/host-to-guest networking, `drafter-agent` and `drafter-liveness` for responding to snapshots and suspend/resume events inside the guest, `drafter-snapshotter` for creating snapshots, `drafter-packager` for packaging VM images, `drafter-runner` for starting VM images locally, `drafter-registry` for serving VM images over the network, `drafter-peer` for starting and live migrating VMs over the network, and `drafter-terminator` for backing up a VM.

## Acknowledgements

- [Font Awesome](https://fontawesome.com/) provides the assets used for the icon and logo.

## Contributing

Bug reports and pull requests are welcome on GitHub at [https://github.com/loopholelabs/drafter][gitrepo]. For more contribution information check out [the contribution guide](https://github.com/loopholelabs/drafter/blob/master/CONTRIBUTING.md).

## License

The Drafter project is available as open source under the terms of the [GNU Affero General Public License, Version 3](https://www.gnu.org/licenses/agpl-3.0.en.html).

## Code of Conduct

Everyone interacting in the Drafter project's codebases, issue trackers, chat rooms and mailing lists is expected to follow the [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/master/code-of-conduct.md).

## Project Managed By:

[![https://loopholelabs.io][loopholelabs]](https://loopholelabs.io)

[gitrepo]: https://github.com/loopholelabs/drafter
[loopholelabs]: https://cdn.loopholelabs.io/loopholelabs/LoopholeLabsLogo.svg
[loophomepage]: https://loopholelabs.io
