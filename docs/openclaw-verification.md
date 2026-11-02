# Native Echo Talk verification

The shared Show/Dot source now includes the subsequent local wake/VAD, ring,
display, network and long-output fixes, plus optional personal agent access.
Before source publication on 2026-11-02, native race-enabled tests passed for
`internal/lib/openclaw`, `internal/config` and `internal/feature/voice`: 325
test/subtest results for Show and 325 for Dot, without failures. Both ARMv7
variants built with Go 1.26.10, CGO disabled, and their exact source inputs were
verified before and after compilation. Binary/ABI/source manifests and device
configuration stay private. Personal access is disabled by default; server
approval and supervised credential provisioning are separate from publication.
The new personal-access binaries have not been installed or acoustically tested.

## Historical candidate verification, 2026-10-26

This record covers the working-tree OpenClaw Talk implementation, based on
TECHO5 main `0255657fd4b7f282bf3415db607665e20780a075` and the sibling Dot main
`b026039c75777197fdb901b8b1a1c55a8c7ed80c`. At that time changes were not committed/pushed.
No Gateway, model or physical device was contacted for speech tests.

## Echo checks

Toolchain: official Go 1.26.10 linux/amd64, archive SHA256 verified before use.
Tests ran in WSL with two Go workers, isolated module/build caches and a 2 GiB
Go memory target. A full race-enabled run passed for all four affected packages:

```bash
go test -p 2 -race -tags dot ./internal/lib/openclaw ./internal/feature/voice ./internal/config ./internal/hardware/speaker
```

Coverage includes synthetic real WebSocket peers, Ed25519 wire signatures,
role/scope separation and receipts, enrollment/token persistence, endpoint
binding, disconnect and in-flight cancellation, clear/replacement fences,
input/end ordering, unavailable speaker rejection, native output burst pacing,
settings export and claim cancellation. Fresh review findings for stale queued
input, playback replacement, post-End capture and already-finished claims were
corrected. After the final PCM-envelope bound, the `TestClaw` group passed again
with Race Detector (2.482 s). The complete earlier race run passed in each
package (client 1.314 s, voice 8.127 s, config 4.130 s, speaker 28.798 s).

The final custom binary was cross-built with `dot`, `linux/arm`, `GOARM=7`,
`CGO_ENABLED=0`, `-trimpath`, `-ldflags=-s`. File inspection confirms a stripped,
statically linked ARM EABI5 executable. VCS metadata records the base above and
`vcs.modified=true`. The ignored local artifact is
`techo5-dot/bin/echod-dot-openclaw` (28,770,466 bytes), SHA256:

```text
e5fb0a2eb5235e8f540131234926c01ec7b3839f09df905a71052ba135b0e20c
```

It is retained as the booted device candidate pending native speech qualification,
not committed to Git. The isolated task-owned Go toolchain, download, module/build caches and
cleanup helper were removed after checking for active consumers. The earlier
Core test wrapper's temporary staging was already absent at closeout. The
independent Core qualification copy has its own closeout. No live release,
rollback material or agent state is changed.

## Supervised Dot 2 installation

One physical `biscuit` was unlocked with the official amonet v2.0.1 package and
prepared with the exact NS6574/7623 Fire OS 6 base in both system slots. Each
boot-critical backup was checked against the device. Recovery material and
device-specific configuration remain private.

The Linux filesystem starts with the project-signature-verified `dot-v1.0.35`
release. Only `usr/local/bin/echod` and `etc/techo5-release` were replaced; all
other 1,781 archive entries retain identical data and metadata. The custom
archive has its own checksum, not an upstream release signature:

```text
da92227bf7578d227ca46cf9fec003f843baa7b351d6d75b5544151839320129
```

The native installer dry run passed before writing. Its boot-image write/readback
check passed, and the real device runs the exact daemon hash above on
`3.18.19-bt`, Linux slot `a`, with the native healthy-boot marker. Capture and
playback devices enumerate. New Wi-Fi configuration was checked by readback;
association, DHCP and the encrypted local API port work after a normal reboot.
The installer's initial network check failed with the unit's old Wi-Fi settings;
the subsequent USB Wi-Fi setup and independent device/network checks resolved it.

Native Gateway enrollment was subsequently qualified on this device. Its USB-read
public identity matches the paired Gateway identity. It has the `node` and
`operator` roles, exactly `operator.talk` for the operator, the `talk` node
capability and no node commands. Both private role tokens were used successfully
after removing the temporary bootstrap credential and its configuration reference
and restarting the device daemon. The native node remained connected, with no
pending device or node-surface request. Existing configuration outside OpenClaw,
including audio and wake settings, matches the pre-enrollment backup.

This qualifies boot, network and enrollment. Matching provider capability,
actual wake/capture, audible replies, interruptions and physical mute remain to
be qualified. No native Talk conversation has been claimed or run.

## Limits

The matching Core/SDK and local-media input-boundary changes require their own
qualification. Their completion, tests/build and later deployment were taken
over by the parallel Core task. No further shared Core writes are made here.
The protocol schema tests passed 7/7 before the pause.

An independent source export based on Core
`adb2388e4c7139cf373a149b76696ff8ec634fe8` plus the nine runtime/test patch files
was used to attempt `native-speech-relay.test.ts`. The canonical harness first
needed its additional versioned build inputs and package-local dependencies.
The final attempt still compiled the common native worker graph; it was stopped
when Core qualification was handed over, before the three new regressions could
execute. This is not a passing Gateway test. All its processes, source exports,
dependency links, build outputs and seven exact test namespaces were removed.
The aborted history-copy clone was also removed. Shared dependency targets,
Core source changes and the final Echo binary were retained.

Do not deploy an Echo against a Gateway missing `audio.supportsInputEnd`.

The existing local-media focused processing/preparation tests passed 30/30.
At that qualification, its package typecheck against the linked Core distribution
was not clean: that SDK was stale for `finishInput` and also had existing
expression/segmentation/admission type mismatches. This is not a validated
combined server release. Core/provider build and actual Echo wake, mute,
playout, interruptions and reconnect remain required before use.

An accidental broader local-media test invocation also observed one
media-provider lease timeout (186 passed, one skipped). That separate failure
was not used as acceptance evidence for this change.

## Generic Core handoff

The Echo requires the existing `talk.session.appendAudio` to accept an optional
`endOfInput: true` with the last real PCM chunk or an empty chunk, gated by
`audio.supportsInputEnd` in the create result. Unsupported providers must reject
the combined request before accepting its audio. Readiness, ownership,
admission, transcript and close fences remain native. No fake silence or timer
change is required. The new three public Gateway regressions cover successful
native consultation, unsupported-provider rejection and close during a pending
input boundary. They remain to be run by the Core owner.

Preserved Core changes from this task:

- `packages/gateway-protocol/src/schema/channels.ts`
- `packages/gateway-protocol/src/channels.schema.test.ts`
- `src/realtime-transcription/provider-types.ts`
- `src/talk/speech/types.ts`, `src/talk/speech/session.ts`
- `src/gateway/talk/speech-relay.ts`
- `src/gateway/talk/handlers/session.ts`
- `src/gateway/talk/handlers/native-speech-relay.test.ts`
- `src/gateway/talk/handlers/native-consult-target.test-support.ts`
- `docs/plugins/sdk-migration/talk.md`
- `docs/gateway/protocol/rpc-talk-config-and-agents.md`

Matching local-media changes are in
`packages/openclaw-local-media/src/realtime-transcription-provider.ts`,
`realtime-transcription-processing.test.ts` and
`realtime-speech-preparation.test.ts` in the extensions repository. Emit the
matching Core SDK, resolve the typecheck and complete native Gateway and
provider qualification before using this as a combined server candidate.

### External provider follow-up

A fresh source-only check on 2026-10-26 confirms that provider `local-media`
(`@infinitecable2/openclaw-local-media`) supplies `finishInput()` on the session
returned by `buildLocalRealtimeTranscriptionProvider(...).createSession(...)`.
`src/index.ts` registers that factory as the realtime transcription provider.
The method exists in both the raw and enhanced-input path: it waits for pending
enhanced frames, ends retained real input through the existing speech/minimum
duration guards, rejects incomplete frames and rejects a pending boundary on
failure/close. Its promise confirms applying the input boundary, not completion
of STT, an agent answer or playback. No additional worker HTTP API is needed.

The locally present `dist/realtime-transcription-provider.js` does **not** yet
contain `finishInput`. The package entry point loads `dist/index.js`, so the
existing built plugin cannot be counted as delivering this capability. After
the matching Core SDK is emitted, the provider must pass its typecheck, be
rebuilt and be installed/loaded as that new artifact in the separately authorized
Core rollout. Verify the selected real session exposes the method and native
create advertises `audio.supportsInputEnd: true`. Existing providers without
the optional method must continue to advertise false. No new Core test copies,
builds or shared Core writes were made for this follow-up.
## Native reply audio qualification (2026-10-28)

The native Talk response now remains 24 kHz PCM16 through transport and reply
buffering, then uses a per-reply stateful converter directly to the existing
48 kHz speaker queue. Microphone capture, wake processing, loudness settings
and speaker tuning were not changed. The packet clock follows sample time,
without accumulating ordinary scheduler lateness; long-stall catch-up is bounded.

Race-enabled native checks passed: 11 audio cases, 33 Gateway client cases and
98 voice cases (including subtests), plus the linux/arm GOARM=7 `dot` build.
Tests cover treble above the old 16 kHz path's Nyquist limit, uneven chunks,
rate-correct buffering/refill, short reply tails, clear/replacement and pacing.

A supervised device-only daemon update preserved configuration and credential
file metadata. The listener reported improved quality and no audible stutter.
One completed physical reply logged 24 kHz, 22.00 seconds of audio and 22.02
seconds of playback, with zero ALSA underruns and zero resampler clipping.
The remaining muffled tone was reported separately; no EQ change was made.
This one listening test is not a general latency or audio-quality benchmark.
