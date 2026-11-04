# Independent onboarding check

This is the manual acceptance test for [#52](https://github.com/Sukhendu2002/boxpool/issues/52).
It requires a person who has not used Boxpool and two supported hosts;
a maintainer following their own guide does not establish independence.
No completed unfamiliar-user run is recorded yet.

## Prepare

Give the participant only the [README quickstart](../README.md), with
its linked docs. Provide two clean supported hosts, their login/sudo
access, and a reachable LAN IP for the head. Ensure the prerequisites
listed at the start are available; record time spent installing missing
prerequisites separately. Use a LAN first; use two hosts on the same
system tailnet for a separate cross-network run. Do not configure Boxpool
for the participant or explain commands while the timer runs.

## Run

1. Start a timer at **Install on both machines**. Observe silently; note
   every clarification requested, wrong command, missing prerequisite,
   wait and recovery. Stop and record a blocker if they cannot continue.
2. Have them install and run doctor on each host, start the head, generate
   a join token, join/label the worker, and verify two `ready` rows with
   `ls --nodes` on the head. A `replica` role on the second row is expected.
3. Have them create `hello` with `--node worker --cpu 1 --mem 512m`, write
   `/root/hello.txt`, exit, and verify it through `exec` on the head.
   Confirm `ls` attributes the running sandbox to the worker. Stop the
   timer; **pass target: less than 10 minutes**, with no verbal assistance.
4. Have them stop and remove `hello` using the documented cleanup. Check
   it disappears from `ls`. Stop daemons if the hosts were only for testing.
5. Separately, with current guest images and Tailscale Serve configured,
   follow the [phone preview](expose.md#check-the-preview) and
   [desktop acceptance](desktop.md#validation) checks. Record image setup
   time separately; these optional features are not part of the two-node timer.

## Record the result

Add the actual observations to the PR or issue; never infer a pass from
unit tests or the author checking commands. Include:

- Date, participant's prior experience, OS/arch/backend of both hosts.
- CLI versions, install method and LAN/tailnet setup.
- Prerequisite setup time, timed onboarding duration and first-download time.
- Exact commands/results for two ready nodes and the worker-owned sandbox.
- Every point of confusion and the doc changes that resolved it.
- Cleanup result, and optional preview/desktop results or why skipped.

Repeat with the revised docs after fixing blockers. Leave #52's
acceptance checkbox open until an unfamiliar-user run meets the target.
