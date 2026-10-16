# VZ snapshot-fork machine identity reproducers

These opt-in diagnostics expose duplicate `VZGenericMachineIdentifier` values
without changing fork or restore behavior. They are skipped in the normal suite.

Apple's `VZGenericPlatformConfiguration.h` requires the original machine
identifier when restoring saved state, but says concurrent VMs with the same
identifier produce undefined behavior in the guest. Changing the identifier in
a snapshot manifest is therefore not a safe fix.

## Configuration-only reproduction

On macOS, prepare the embedded shim:

```sh
make sign-vz-shim
```

Then run:

```sh
HYPEMAN_VZ_IDENTITY_REPRO=1 go test -tags containers_image_openpgp \
  -run '^TestPrepareForkMachineIdentifierCollisionRepro$' \
  -count=3 -v ./lib/hypervisor/vz
```

This creates a real VZ identifier, serializes it into a manifest, runs
`PrepareFork`, and reconstructs a VZ platform from the resulting identifier.
The final inequality assertion intentionally fails on the current implementation:
source and fork have the same platform identity.

This does not boot a VM, prove that two VMs are running, or demonstrate any
guest-visible damage. It can run on a virtualized Mac without nested
virtualization support.

## Live reproduction

Use an Apple Silicon Mac running macOS 14+ that can actually launch VZ guests.
A macOS VM without nested virtualization cannot run this test.

```sh
brew install e2fsprogs
make build-embedded sign-vz-shim
PATH="/opt/homebrew/opt/e2fsprogs/sbin:$PATH" \
  HYPEMAN_VZ_IDENTITY_REPRO=1 go test -tags containers_image_openpgp \
  -run '^TestVZConcurrentForkMachineIdentifierRepro$' \
  -v -timeout 10m ./lib/instances
```

The test boots a source, puts it in standby, forks it into another standby
instance, and reads both saved identifiers before restore consumes the manifests.
It restores both guests and verifies they are running and can execute commands
before asserting that their identifiers differ. The intended diagnostic failure
is at that final assertion, not during setup or restore.

A failure such as `Virtualization is not available on this hardware` is an
environment blocker, not a reproduction of concurrent execution. Neither test
claims that duplicate identifiers necessarily cause crashes or corruption.

## CLI equivalent

With a running VZ instance named `source`:

```sh
hypeman fork --from-running --target-state Running source copy
```

The current implementation preserves memory in both guests and preserves their
VZ platform identifier. A collision-safe implementation must explicitly decide
whether to reject simultaneous execution or require a disk-only, cold-booted
copy. Silently throwing away saved memory would change the operation's meaning.

The existing explicit cold-boot path is:

```sh
hypeman fork --from-running --target-state Stopped source copy
hypeman start copy
```

This retains the copied disk state, not the fork's saved RAM or running processes.
The fresh VZ launch generates a new machine identifier.
