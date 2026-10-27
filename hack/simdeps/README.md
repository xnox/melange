# simdeps: simulate versioned "depends:" across the archive

`simdeps` rewrites APKINDEX files so that they show what the indexes
would contain if every package were rebuilt with versioned shlib and
pkg-config depends. `run.sh` then checks whether stereo's
`make compile-all` and `make lock-update` still resolve against them.

## Running it

```sh
hack/simdeps/run.sh            # everything: index, stereo, baseline, sim, report
WORK=/big/disk OPTOUT='hwloc-cuda-*,zlib-ng*,cyrus-sasl*' hack/simdeps/run.sh
hack/simdeps/run.sh report     # just re-print the comparison
```

Requirements:
- `chainctl` auth for apk.cgr.dev
- `art` on `PATH`
- a stereo checkout (`STEREO`, default `~/chainguard-dev/stereo`)
- about 2 GB free in `WORK`

The stereo checkout is cloned with `--shared`, never modified. Fetched
`.melange.yaml` files are cached in `$WORK/sim/cache`, so a rerun takes
about a minute plus the stereo runs.

`OPTOUT` simulates a provider-side opt-out: packages whose origin
matches one of the globs are treated as publishing no `so-ver:`
provides, so their consumers keep plain `so:` depends.

## How it works

1. For the newest build of each package name in each repository, it
   range-fetches only the APK's control section and reads its recorded
   `.melange.yaml`. The `environment.contents.packages` pins in that
   file are exactly what the build had installed.
2. For each unversioned `so:X` or `pc:X` depend, it applies the logic of
   `determineProviderVersion` (`pkg/sca/sca.go`). If the pinned provider
   publishes a versioned provide, it adds `so-ver:X>=V` or `pc:X>=V`,
   where V is the pinned version. A provider from the package's own
   origin counts as `@CURRENT@`. It skips explicit runtime depends,
   depends that are already versioned, and packages with
   `no-versioned-shlib-deps`.
3. It writes the new indexes and `<arch>-report.tsv`, which lists every
   added constraint and every resolve error. `run.sh` signs the indexes
   with a throwaway key, points a stereo clone (the repository constants
   in `cmd/stereo/main.go`, and the files in `stereo-cue-files.txt`) at
   them, and diffs a baseline run against a simulated one.

Checks: a round trip with no changes reproduces the Wolfi APKINDEX byte
for byte, and the output matches `melange scan --diff` (see
`pkg/cli/scan_test.go`).

## Results, 2026-10-27 (no opt-outs)

| | Real indexes | Simulated | New |
|---|---|---|---|
| Package build environments that fail to resolve (x86_64, ~10.4k packages) | 1 | 36 | **35** |
| Package build environments that resolve to a different package set | | 72 | |
| Image modules that fail to resolve | 8 | 31 | **23** |
| Images whose locked package set changes | | 30 | |
| Image lock step failures | 5 | 5 | 0 |

### Root cause: one soname, several providers, unrelated version scales

`so-ver:<soname>=<pkgver>` ties a soname to its provider's package version. When ABI-compatible alternatives provide the same soname, the `>=` floor recorded against one of them can never be met by the others:

| Provide | Competing providers | Example floor that can't be met |
|---|---|---|
| `libpq.so.5` | `libpq-14` … `libpq-18` | a `libpq-17` consumer against `>=18.6-r5` |
| `libsasl2.so.3` | `cyrus-sasl` MIT vs Heimdal builds (r5 vs r58) | `>=2.1.28-r58` |
| `libgcrypt.so.20` | `libgcrypt` 1.12.x vs a FIPS drop-in at 0.13.x | `>=1.12.4-r0` |
| `libhwloc.so.15` | `hwloc` vs `hwloc-cuda-*` | `>=2.14.0-r0` |
| `libzmq.so.5` | `zeromq` vs its variants | `>=4.3.5-r13` |
| `libcryptsetup.so.12` | `cryptsetup` vs its FIPS variant | `>=2.8.8-r3` |
| `librados.so.2` | ceph-19 vs ceph-20 | `>=20.2.4-r4` |
| `libz.so.1` | `zlib` vs `zlib-ng-compat` | `>=1.3.2.1_rc…` |
| `pc:absl_*` | the `abseil-cpp-YYYYMMDD` streams | `>=20260817.0-r4` breaks grpc-1.66 … 1.70 |
| `ld-linux-*.so` | glibc 2.43 vs 2.44 | consumers that pin an older ld.so |

A few failures look like **genuine floors** that unversioned `so:` depends were hiding. Example: a FIPS provider pinned to glibc 2.41/2.42 meets an openssl build that now needs `so-ver:libc.so.6>=2.44-r9`. An older ncurses meets a readline that needs `so-ver:libtinfo.so.6>=6.6.20260822`. These are arguably the feature working as intended.

### The silent changes may matter more than the failures

When resolution still succeeds, the floors move the solver onto different providers, and no step fails:

- In package build environments, `icu` drops from 79.1 to 78.3 in 33 packages, `openssl-3.5` is replaced by `openssl-3.6` in 15, and `grpc-1.84` is replaced by `grpc-1.85` in 8.
- In images, `openssl-3.5` is replaced by `openssl-3.6` (about 110 libcrypto lock lines), `cryptsetup` by its FIPS variant, FIPS images pick up different openssl provider and config packages, and `icu` changes.

### Separate bug: `determineProviderVersion` sorts candidates by build time

`libmspack` went from `1.11-r0` back to `0.11_alpha-r5`/`-r6`, with the higher version built *earlier*. `determineProviderVersion` sorts candidates by `BuildTime`, newest first, so it sees `0.11_alpha-r6`. That is older than the pinned `1.11-r0`, so it returns an error, and the build fails at SCA time for cabextract, rizin, rizin-dev, clamav-1.0/1.4/1.5 and open-vm-tools (12 errors per arch). It should compare by version, or skip candidates older than the pin rather than erroring.

### Decisions so far

- **Keep versioned:** `glibc-*` and `openssl-3.*`. They are split into
  origins only for rebuilds and upgrades, and form one compatible
  stream. A binary built against 3.6 should gain
  `so-ver:libcrypto.so.3>=3.6`, and one built against 3.5 should gain
  `>=3.5` and work with either. The resulting openssl-3.5 to 3.6
  switches, and the old-glibc floors, are intended.
- **Opt out on the provider side** (same ABI, versions on unrelated
  scales): the cuda variants (`hwloc-cuda-*`), `zlib`/`zlib-ng`, and
  `cyrus-sasl`. This needs a provider-side option that melange doesn't
  have yet (for example `no-versioned-shlib-provides`), and the
  providers have to be rebuilt before the default flips.
  The option must also drop versioned depends between the provider's
  own subpackages (the `@CURRENT@` case): without `so-ver:` provides
  they could never be satisfied. `simdeps -no-versioned-provides` does
  this.
- **Still to decide:** `libpq-NN`, the FIPS drop-ins for `libgcrypt`
  and `cryptsetup`, the `zeromq` variants, `ceph-19`/`ceph-20`, and
  `pc:absl_*`, which a `so-ver:` opt-out doesn't cover.

For scale: 1,150 of the 5,645 `so-ver:` sonames on x86_64 (about 20%)
have providers from more than one origin, across 334 origins.
