# M1 spike: Firecracker cold boot time

Issue #2. How long does a Firecracker microVM take to cold boot to a shell
with the upstream quickstart kernel and rootfs, and is under 2 s realistic?

**Status: incomplete.** The only KVM machine available so far is a
GitHub-hosted runner, which is itself a VM (nested virtualization). Its
numbers are below and are **not reference numbers**. The reference Linux box
and the old laptop rows are pending.

## Method

[`scripts/bench-boot.sh`](../../scripts/bench-boot.sh) follows Firecracker's
[getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
with pinned artifacts:

| Artifact | Pin | sha256 (x86_64) |
| --- | --- | --- |
| Firecracker | release `v1.17.0` | `06094a11…9ade558`, matches upstream's `.sha256.txt` |
| Guest kernel | `vmlinux-6.18.54` from CI prefix `firecracker-ci/20261016-5732b8331e1e-0`, 27,893,008 bytes | `445b3d0e…1910f0d`, observed (no published checksum) |
| Rootfs | `ubuntu-24.04.squashfs` from the same prefix, 108,228,608 bytes | `40eaeb2c…f58956e`, observed (no published checksum) |

The guide picks the newest CI prefix at run time. The script pins the prefix
the guide selected on 2026-10-18, so every machine boots the same bytes. Full
hashes for x86_64 and aarch64 are in the script. As in the guide, the
squashfs is unpacked as root into a 1 GiB ext4 image (428,660 KiB used). The
guide's SSH key step is skipped because only the serial console is used.

Each run copies the ext4 image fresh, starts `firecracker --api-sock …
--enable-pci --boot-timer`, then configures it through the API socket with
`curl`: `machine-config` (2 vCPU, 1024 MiB), `boot-source`, `drives/rootfs`,
then `actions` `InstanceStart`. Firecracker's stdout is the guest serial
console and goes to a file. Firecracker writes and flushes it one byte at a
time. Firecracker's own log goes to a separate file. Afterwards the script
kills Firecracker and deletes the run's files, including on failure.

Boot args are the guide's `console=ttyS0 reboot=k panic=1`, plus
`init=/usr/local/bin/init`. That init is shipped in the quickstart rootfs: it
signals Firecracker's boot-timer device, then execs systemd. The rootfs has
an autologin getty on `ttyS0`, so the boot ends at a root shell prompt
(`root@ubuntu-fc-uvm:~#`).

Measurements, in ms:

| Column | From | To |
| --- | --- | --- |
| `api` | Firecracker process start (just before exec) | the `InstanceStart` request returning. Includes four `curl` processes, about 4 ms each |
| `first` | process start | the first byte from the guest on the serial console |
| `prompt` | process start | the root shell prompt on the serial console |
| `boot-timer` | Firecracker receiving `InstanceStart` | the guest init writing the boot-timer address (Firecracker's `Guest-boot-time` log line) |
| `kernel-init` | the guest kernel's clock at zero | `Run /usr/local/bin/init as init process` (printk timestamp) |

`first` and `prompt` come from polling the console file every 5 ms. They can
be late by about 5 ms, which does not matter at this scale. Runs use a warm
host page cache: the image is copied before each boot, and the script does
not drop caches. A true cold-disk boot is not measured.

`api` + `boot-timer` is roughly when systemd starts. `prompt` minus that is
systemd's time to the autologin shell. `boot-timer` minus `kernel-init` is
Firecracker setup plus the kernel's early, unclocked boot.

The first nested run showed the kernel idle for about 450 ms between
`check access for rdinit=/init failed` and
`input: AT Raw Set 2 keyboard`: the legacy i8042 keyboard probe. Firecracker's
own boot-time test turns it off. So the workflow also measures a second
configuration that adds
`i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd`, with the same artifacts.

## Reproduce

On any Linux x86_64 or aarch64 machine with read/write `/dev/kvm` (see
[development.md](../development.md)), with `curl`, `squashfs-tools`,
`e2fsprogs`, bash 5+, and `sudo` for the one-time rootfs conversion:

```sh
git clone https://github.com/Sukhendu2002/boxpool.git && cd boxpool
scripts/bench-boot.sh 10 | tee bench-guide.txt
EXTRA_BOOT_ARGS='i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd' scripts/bench-boot.sh 10 | tee bench-i8042.txt
```

The script prints host facts (CPU, `nproc`, memory, OS, kernel, whether the
host is virtualized), the artifact sizes and hashes, each run, and the
median/p90. Set `LOG_DIR=dir` to keep every run's console and Firecracker
log. Without `/dev/kvm`, it exits 1 and says why. That is the "no KVM"
answer for a machine.

On GitHub: **Actions → Boot benchmark → Run workflow** (manual only). It runs
both configurations on `ubuntu-24.04` and puts the output in the job summary
and an artifact.

## Results

Median / p90 in ms over 10 boots. p90 is nearest-rank (the 9th of 10 sorted
values).

**Rows marked "nested" ran in a nested VM on a GitHub-hosted runner, not
reference hardware.** Each runner is an Azure VM (`systemd-detect-virt`:
`microsoft`) with 4 vCPU and 15.6 GiB. It runs Ubuntu 24.04.5 on host kernel
`6.17.0-1022-azure`. Firecracker runs inside it on nested KVM. Each sample
landed on a different physical CPU model.

| Machine | Boot args | api | first | prompt | boot-timer | kernel-init |
| --- | --- | --- | --- | --- | --- | --- |
| Nested, AMD EPYC 9V74 ([run 1](https://github.com/Sukhendu2002/boxpool/actions/runs/39068917672)) | guide | 26 / 26 | 38 / 39 | **1608 / 1674** | 945 / 970 | 854 / 873 |
| Nested, Intel Xeon Platinum 8573C ([run 2](https://github.com/Sukhendu2002/boxpool/actions/runs/39069037608/attempts/1)) | guide | 30 / 35 | 46 / 49 | **1621 / 1646** | 987 / 991 | 893 / 894 |
| Nested, Intel Xeon Platinum 8573C (run 2) | guide + i8042 off | 29 / 31 | 44 / 47 | **1150 / 1168** | 491 / 514 | 397 / 417 |
| Nested, AMD EPYC 7763 ([run 3](https://github.com/Sukhendu2002/boxpool/actions/runs/39069037608/attempts/2)) | guide | 31 / 34 | 44 / 47 | **1918 / 1965** | 1084 / 1104 | 973 / 984 |
| Nested, AMD EPYC 7763 (run 3) | guide + i8042 off | 31 / 34 | 45 / 49 | **1470 / 1516** | 592 / 634 | 486 / 520 |
| Reference Linux box | guide | pending | pending | pending | pending | pending |
| Reference Linux box | guide + i8042 off | pending | pending | pending | pending | pending |
| Oldest supported laptop (KVM: pending) | guide | pending | pending | pending | pending | pending |
| Oldest supported laptop | guide + i8042 off | pending | pending | pending | pending | pending |

Medians of 10 values are the mean of the middle two, rounded here to the
nearest ms. Run 1 predates the i8042 configuration. Runs 2 and 3 are two
attempts of the same commit.

Hardware to record when the pending rows are filled in: CPU model, cores,
memory, disk, distribution and host kernel (all printed by the script). For
the old laptop, also record whether `/dev/kvm` exists at all.

### Raw runs (nested)

Columns: run, api, first, prompt, boot-timer, kernel-init (ms).

```text
run 1, EPYC 9V74, guide       run 2, Xeon 8573C, guide      run 2, Xeon 8573C, i8042 off
 1  27 40 1612  948 848        1  41 53 1646  998 896        1  30 43 1167 492 401
 2  25 33 1582  946 853        2  27 41 1626  982 893        2  27 40 1099 472 383
 3  26 39 1659  970 873        3  31 44 1605  989 892        3  29 47 1158 502 408
 4  25 38 1581  906 820        4  31 45 1615  965 864        4  28 41 1142 468 379
 5  25 38 1705  965 872        5  35 48 1660  991 894        5  33 46 1158 518 419
 6  26 33 1649  942 854        6  29 43 1640  988 893        6  28 41 1055 452 364
 7  25 38 1604  926 838        7  28 47 1570  985 891        7  31 44 1168 514 417
 8  26 33 1595  944 854        8  30 49 1606  956 860        8  29 48 1137 492 397
 9  26 39 1584  934 840        9  28 47 1626  988 894        9  28 41 1173 487 395
10  26 39 1674  970 877       10  30 43 1576  958 860       10  28 47 1126 490 396

run 3, EPYC 7763, guide       run 3, EPYC 7763, i8042 off
 1  34 47 1918 1053 951        1  30 43 1483 649 531
 2  34 48 1915 1085 973        2  32 45 1509 634 520
 3  32 45 1912 1065 948        3  34 49 1460 597 483
 4  29 42 1965 1104 984        4  31 45 1450 585 474
 5  31 45 1947 1088 977        5  31 45 1516 571 471
 6  29 42 1905 1043 949        6  31 45 1466 594 489
 7  32 45 1904 1083 972        7  30 43 1474 588 488
 8  29 42 1992 1109 986        8  35 49 1450 576 474
 9  30 43 1918 1098 984        9  29 43 1444 590 483
10  30 43 1931 1061 947       10  32 45 1538 617 504
```

## Conclusion (provisional, nested runner only)

On a nested VM, the quickstart guest reaches a root shell in 1.6–1.9 s
(median; worst p90 1.97 s) with the guide's boot args. With the i8042 probe
off, it takes 1.15–1.47 s. So under 2 s already holds on the runner, which
probably handicaps the guest. What drives the time is the guest, not
Firecracker. `InstanceStart` returns about 30 ms after process start, and the
first console byte follows 10–15 ms later. The kernel then takes about
0.85–0.97 s to reach init. About 0.5 s of that is the legacy i8042 keyboard
probe, which a boot argument removes. Systemd and the getty then take another
0.6–0.85 s to reach the shell. The 428 MB rootfs is read lazily from a warm
page cache, so these runs show no rootfs-size effect, and a cold-disk boot
was not measured.

What the nested numbers **cannot** tell us: absolute bare-metal times (every
VM exit, including each serial console byte, costs more under nested
virtualization), the spread between machines (three runner CPUs varied by
about 300 ms), and anything about the old laptop, including whether it has
KVM at all. What they **can** tell us: the breakdown, that Firecracker
overhead is negligible, that the guide's boot args leave about 0.5 s of
kernel time on the table, and that "about a second" needs both that fix and a
lighter init than full Ubuntu systemd. Building that image is the image
issue's job. Fill in the pending rows before treating any of this as decided.
