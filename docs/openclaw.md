# Echo as a native OpenClaw Talk device

This fork adds a selectable Gateway transport to `echod`. The Echo owns its
existing microphone, local wake/endpoint detector, button/mute and speaker.
OpenClaw owns STT, agent conversation, tool policy and TTS. Home Assistant is
not required for this backend. There is no device-side LLM or second agent loop.
This is a source/build candidate, not a hardware-qualified flash release.
See the [local verification record](openclaw-verification.md) for checks and limits.

## Minimal route

```text
existing TECHO5 wake/capture -> native Gateway Talk RPCs (16 kHz mono PCM16)
 -> native SpeechSession -> configured STT -> native agent -> configured TTS
 -> talk.event (24 kHz mono PCM16) -> existing TECHO5 conversion/player
```

One persisted Ed25519 identity opens the two connections used by the native
Android model: `node` with no operator scopes, and `operator` with exactly
`operator.read` and `operator.talk`. Registering a node alone does not authorize Talk RPCs. Each role
keeps its own device token. The node advertises `talk` and no remotely invokable
commands. The Gateway requires `operator.read` for delivery of `talk.event`,
including targeted audio, transcripts and terminal errors; `operator.talk` alone
can send RPCs while all those replies are filtered. Read is a Gateway-wide read
grant, not a Talk-only permission, and requires explicit operator approval.
The client accepts exactly those two operator scopes, rejects duplicates or
additional grants, forwards only Talk events and exposes only Talk RPC calls.
No admin, general write or secret scope is requested. Tool authority remains
subject to native Talk policy and this scope set.

Each local turn creates the following native media session:

```json
{"mode":"stt-tts","transport":"gateway-relay","brain":"agent-consult","sessionKey":"agent:example:voice:echo-example"}
```

Select a device-specific key for an existing configured agent; the example name
is fictional. That persistent key retains native history between media sessions.
It does not imply access to an owner's private main or Matrix conversation.
The exact `readyMark` is acknowledged only after the real speaker is acquired.
Readiness is not proof that future audio was physically played. Bounded buffers
retain real capture during cold setup and pace native TTS bursts into the
existing player. Overflow fails visibly. Clear/cancel and replacement turns
fence queued input, output and playback callbacks. Mutations are not blindly
replayed after a disconnect or a canceled call.

## Required matching Core and provider

Use the Core candidate described in
`openclaw/docs/plugins/sdk-migration/talk.md`. Its create result must declare
`audio.supportsInputEnd: true`; otherwise this backend refuses capture before
acknowledging readiness. The existing Echo endpoint stops capture locally;
missing network data is not silence. End therefore uses the existing RPC:

```json
{"sessionId":"returned-session-id","audioBase64":"","endOfInput":true}
```

This small optional native contract flushes real retained input through
`SpeechSession.finishInput` and the provider's optional `finishInput`. It does
not manufacture silence, close the session early, bypass speech/admission
guards or change endpoint timers. The matching `openclaw-local-media` adapter
implements this boundary including enhanced-frame drain. Build the matching
Core/SDK and provider together and qualify them before deployment.

Agent Talk profiles, STT/TTS providers, voices and worker admission stay on the
Gateway. The Echo adds no speech-processing/model parameters. Installing this
binary does not update the Gateway, plugin, models or workers.

## Configuration and operator renewal

On a Dot, merge this into `/data/misc/echolocal/state.json` while its daemon is
stopped, preserving other settings, then restart through its existing supervisor:

```json
{
  "openclaw": {
    "url": "wss://gateway.example",
    "session_key": "agent:example:voice:echo-example",
    "state_dir": "/data/misc/echolocal/openclaw",
    "bootstrap_token_file": "/data/misc/echolocal/openclaw-bootstrap"
  }
}
```

This startup configuration has no new setup UI. Use WSS with a trusted valid
certificate over LAN/remote networks. Plain WS is accepted only for exact
loopback `localhost`, `127.0.0.1` or `::1`. Loopback also permits same-host
development; it does not establish that a full Gateway/model stack fits an
Echo's limited RAM. The connections require no new inbound device port.

1. Retain local wake settings. HA wake streaming/confirmation configured together
   with OpenClaw is rejected at startup instead of silently reconfigured.
   Incomplete OpenClaw configuration fails instead of falling back to another
   assistant.
2. For an already enrolled node, the Gateway owner issues a short-lived native
   device bootstrap credential with roles `operator`, scopes `operator.read`
   and `operator.talk`, and purpose `voice-node`. Provision it through the
   approved private commissioning channel into the bootstrap file, exact content
   with no newline and mode `0600`. Never use a shared Gateway secret, or put
   credential values in JSON configuration, arguments, Git or logs. State must
   be a real `0700` directory with regular `0600` files. Preserve the existing
   identity and node token; temporarily retain only the old operator token on
   the device and remove it from the active filename for this explicit renewal.
   The default bootstrap role is `operator`, retaining the renewal path.
   For first enrollment, use OpenClaw's native `voice-node` setup profile and
   temporarily set `bootstrap_role` to `node`. The Node connection redeems the
   setup credential. Follow the Gateway's explicit device/role approvals for
   Linux Echo hosts; do not impersonate another hardware family to obtain a
   silent handoff. If the Gateway supplies a complementary operator grant in
   `hello-ok`, the client accepts exactly `operator.read` and `operator.talk`.
   Both grants are validated before storage. A changed existing companion grant,
   extra roles/scopes, or an unsolicited companion receipt is rejected. Operator-only
   renewal preserves the existing Node credential and its metadata.
3. Inspect the pending request on the Gateway host, compare the existing public
   device identity, and explicitly approve only that operator scope upgrade.
   Review the Gateway-wide read grant before approving. The existing node keeps
   empty scopes; the operator requires exactly `operator.read` and
   `operator.talk`. Cached-token rejection never falls back to bootstrap.
4. Confirm both role-token files and readiness. Remove the bootstrap file and
   its config reference and `bootstrap_role`, restart and verify readiness using
   only the role tokens.
   A successfully delivered bootstrap hello consumes the setup credential before
   local token persistence is acknowledged. If persistence fails, stop and have
   the owner issue a fresh operator-only credential; do not replay the old one.
   A software rollback alone cannot restore an old scope-specific token after
   native grant/token changes; coordinate that recovery with the Gateway owner.
5. Qualify real wake/button input, the complete spoken answer, clear, cancellation,
   reconnect and physical mute on the selected hardware before unattended use.

`gateway.url` binds private state to its enrolled endpoint. A different endpoint
requires deliberate enrollment in a new state directory. The complete binding
is device-local and excluded from settings exchange; imports cannot redirect
the paired device. Leave upstream automatic firmware installation disabled
while testing a custom binary, since an upstream update would replace it.
The code does not redirect updates to an unverified feed.

## Native turn timing and ring

Each wake slot has an independent `no_speech` opening window (1–60 seconds,
default 10 for older settings). Sustained fresh microphone activity or an
accepted transcript ends that guard; the existing maximum request length and
processing limit then apply. Opening/follow-up timeout plays the short cancel
tone (subject to existing quiet hours), without a failure animation.
Waiting music received during open native capture cannot take reply ownership
or close the microphone; it is used only once the device is processing.

The existing `follow_up` setting controls keyword-free listening after the
answer has finished playing locally (0 disables it, otherwise 1–30 seconds).
Setting both windows to 10 gives ten seconds to start the first request and
ten seconds to continue after each answer. `follow_ups` still limits consecutive
follow-ups when nonzero. Both windows are persisted per slot and exposed as
separate device settings.

Native endpointing waits for confirmed fresh input activity before it may end
capture; the relative detector alone cannot turn an empty opening into a
processing request. The adaptive room gate confirms 200 ms of activity across
syllable gaps shorter than 160 ms; isolated separated noise does not accumulate.
A nonempty native `transcript.delta` also confirms input, while raw PCM receipt
does not. Confirmation disarms the opening guard once and starts the request
budget; repeated activity cannot extend that budget.
The native relative endpoint is also gated by that room comparison: softer
ongoing speech may fall below the request's initial loud peak, but capture
does not end until the room gate has stayed inactive for the configured pause.
Activity during a pause resets that quiet interval. Legacy endpoints are unchanged.
`end_silence_ms` controls the native relative pause in
100 ms windows (200–3000 ms; zero keeps the legacy 600 ms). For example,
`max_listen: 60` and `end_silence_ms: 1200` allow up to one minute from speech
onset while spanning shorter thinking pauses. At the capture cap, a native
request that has begun is submitted rather than discarded as a failure.

With no explicit phase-effect overrides, native Talk shows a rotating comet
while processing, including waiting music, and a solid ring for agent speech.
Speech is identified by native `output.audio.started`/`output.audio.delta`
events, kept in output order. `output.audio.done` labels the end of a speech
span without ending the answer. Local queued samples plus a hardware-tail
margin determine when speech is audible; drained pauses and waiting music
return to rotation. Only the final locally played reply opens follow-up.
Final audio drain does not flash processing before that listening window;
its listening effect resumes when capture actually reopens.

Native replies have no fixed total playback duration or five-minute turn cap.
While waiting for output, `max_think` is an inactivity budget: accepted PCM
refreshes it, including waiting audio. Queued PCM protects a racing timeout;
speech markers or a live WebSocket alone do not prove audio progress. A true
stall still expires. `turn.ended` closes the ordered audio stream, removes the
processing timer and lets the local player drain before follow-up opens.
The transport backlog remains bounded at 8192 events (about 164 seconds when
all are 20 ms PCM frames), enough for a two-minute TTS burst. This bounds pending
audio rather than cumulative reply length; overflow fails explicitly.

These changes do not alter capture, wake scoring,
echo cancellation, gain or reply conversion.

On a Dot, `ring.auto_brightness` optionally scales all ring layers using the
existing safe Lux reader, without lighting an otherwise dark ring. The curve
ranges from 20% in darkness to 100% at 250 lux, multiplying manual brightness;
half-second readings are smoothed, dimming more slowly than brightening.
Unknown readings fall back toward full brightness. The separate hardware
microphone privacy LED is unaffected. A device switch can disable the scaling.

The native default `Pulse` listening appearance also visualizes the existing
adaptive microphone level: a low breathing baseline leaves brightness headroom
for louder activity, under the same room-light scale and configured RGB color.
Short raw gate crossings remain visible for 160 ms at the 25 fps render rate.
A non-destructive activity-frame counter prevents a short crossing from being
missed between renders. The visual hold never feeds onset/endpoint decisions.
Brightness shows acoustic activity; successful onset confirmation remains a
separate state. Processing and agent speech keep their existing appearances.

Wakeword detection remains active during output: a wake retires the old local
reply/native session before opening new capture. The independent local stop
word cancels output; no wakeword is required for it. Stop acceptance and the
failed native RPC method are logged without transcript content so unexpected
interruptions can be distinguished from transport failures.

## Build

Native Talk reply audio remains PCM16 mono at 24 kHz through the relay and
reply buffer. A per-reply, stateful band-limited converter feeds the existing
48 kHz stereo speaker queue directly; it does not pass through the legacy
16 kHz Home Assistant voice player. Microphone capture and wake processing
keep their existing rates. If a reply is also sent into an active telephone
call, only that separate branch is converted to its 16 kHz voice rate.

Reply pacing follows the original sample timeline so small scheduler delays
do not accumulate across packets. Catch-up after a long stall is bounded.
The configured initial cushion and 80 ms refill cushion remain in use,
counted at the actual reply rate. Playback diagnostics report `rate_hz`,
duration, excess playback time (`gap`), queue seams, ALSA underruns and
resampler clipping. These counters complement a physical listening test;
they do not prove acoustic delivery.

Use Go 1.26+ and existing module dependencies:

```bash
cd techo5/echod
go test -race -tags dot ./internal/lib/openclaw ./internal/feature/voice ./internal/config ./internal/hardware/speaker
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -tags dot -trimpath -ldflags=-s -o ../../techo5-dot/bin/echod-dot-openclaw ./cmd/echod
```

`dot` selects existing Dot 2 board support. Dot 3 requires `dot,donut` and its
own qualification. See [Dot packaging](../../techo5-dot/docs/openclaw.md).
A Show 5 uses the same source without the `dot` tag. That selects its own
microphone array, playback references, speaker and display drivers. The Nova
model is a separate deployment artifact; enable its matching exact frontend
with `TECHO5_WAKE_FRONTEND=exact`. Local VAD, capture/turn timing and the Talk
transport are shared. Keep microphone tuning and equalization device-specific.
The existing screen can show reply text and state; word-level synchronization
with audio is not implemented by this backend.
A cross-build alone is not a boot/flash test. No Go dependency is added.
The upstream MIT license remains; preserve all separate kernel/driver/input
notices when distributing images. This daemon does not relicense Amazon's
per-device firmware.

## Show reading focus

With OpenClaw selected, the local clock and settings become available after
the normal minimum splash time, independently of Gateway connectivity.

The reading position is projected onto the actual wrapped answer lines at the
current font size and resolution. Its target is the middle of the usable
answer area, excluding the clock/footer margins and the still-visible question.
Once the question scrolls away, the whole text viewport supplies that area.
Both timed word positions and the provisional reading clock use this geometry.
Automatic scrolling only advances, respects the content limits and yields to
manual swipes. Near the beginning or end, those limits can prevent centering.

## Audit of the earlier Pi Voiceassistant

The Pi path is already native at the speech/agent layer. Its device service
uses `prepare/startMeetingAgentRealtimeEngine`; these compatibility facades
enter `startSpeechSession` in `openclaw/src/talk/speech/session.ts`. It has no
independent agent loop. Device capture, AEC, wake, mute and transport admission
are adapter responsibilities and remain useful.

There is avoidable duplication in its speech-profile/provider configuration and
presentation options. Native `resolveTalkSpeechAgentProfile` already supplies
the common profile. The smallest next simplification is to resolve that profile
directly and remove duplicated defaults after parity tests. This audit does not
justify rewriting working Pi audio.

The Echo uses a thinner public transport: direct Talk RPCs without a
device-specific server media service. Migrate Pi only after continuous turns,
barge-in and confirmation parity are proven. This Echo candidate creates one
media session per existing local turn and does not advertise full-duplex or
output-gating capabilities. The Pi README's implication of the same private
Matrix/main session needs correction: its current conversation is device-scoped.
Likewise, queue acceptance is not a physical playback receipt. No Pi deployment
is changed here.
