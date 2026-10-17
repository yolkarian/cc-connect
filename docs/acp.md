# Native ACP model controls

The ACP adapter reads model and reasoning choices from the active session's
`configOptions`. It does not shell out to a provider-specific model command,
maintain a static catalog, or guess which thinking levels a model supports.

## Commands

- `/model`: show the actual active model and a paginated native model picker.
- `/model page 2`: navigate the picker; the first open shows the active model's page.
- `/model switch provider/model-id`: change the active model in place.
- `/model short-id`: use an unambiguous native model alias.
- `/reasoning`: show the active model's advertised thinking choices.
- `/reasoning max`: select `max` only when the backend advertises it.

Text commands, Telegram inline buttons, and structured card actions use the
same session controller. Picker callbacks identify models independently of
catalog order and fit Telegram's 64-byte callback limit.

A switch sends standard `session/set_config_option` using the backend's actual
config ID. Success is shown only after the response confirms the requested
value. The returned options replace the cached snapshot: switching models can
change or clamp the available reasoning levels. `config_option_update`
notifications refresh the same state without becoming answer chunks. Footer
readers use the live session's model and effort rather than startup defaults.
OMP role selectors can include a `:thinking` suffix; ACP model choices do not.
Select `/model provider/model-id` and `/reasoning max` separately instead of
passing a role selector to the conversation-model picker.

## Conversation continuity and scope

Model and reasoning changes retain the native session UUID, bridge history,
and running process. Changes during an active prompt are rejected rather than
interrupting that turn. **The backend is the only persistence owner** for native
session selections. The bridge caches acknowledged metadata for display, but
does not write/replay workspace model or effort preferences. Each workspace's
bound native UUID restores its own saved selection. Changing a conversation
model does not implicitly change defaults for future conversations.

An explicit resume failure is returned to the user. It does not trigger a new
conversation or erase the saved UUID, including relay sessions. The adapter
uses advertised `session/close` support to flush native state before stopping
its subprocess; close is idempotent.

## OMP 18.8.6

Use the official CLI with `cmd = "omp"`, `args = ["acp"]`,
`auth_method = "agent"`, and ACP `mode = "default"`. Tool approval remains a
separate native setting (`tools.approvalMode: yolo`). Optional ACP constructor
defaults are `model` and `reasoning_effort`, applied **only to session/new**.
They are never reapplied to session/load or changed by conversation pickers.
Omit them to let native `modelRoles.default` own new-conversation defaults.
For workspace-specific future defaults use native `.omp/config.yml`, not a
bridge preference store. Role/default controls remain separate operations.

OMP exposes categories `model` and `thought_level`, including its model-specific
`max` selector. The picker controls the conversation model; configure smol,
task, and advisor roles using native OMP `modelRoles`, not a Pi compatibility
shim. Preserve additional roles when replacing that record.

OMP 18.8.6 does not advertise role configuration in ACP `configOptions`. Do not
send invented `configId` values such as `task` or `smol`. Native `omp config`
changes routing defaults; an ACP model/effort switch does not write those roles.
Project and CLI-overlay assignments can override global defaults. Native task
routing reloads role changes before subsequent launches; active subagents keep
their models. A new OMP process picks up a changed `default`, while a resumed
session keeps its saved selection. OMP can retain its startup default for
additional `session/new` requests inside the same ACP process; the bridge
starts a fresh process for each new conversation.

If OMP cannot restore a saved model whose credential is unavailable,
`session/load` fails *before* a model config option can be changed. The bridge
surfaces OMP's `data.details` error and retains the binding. This requires
reauthorization or an explicit, backed-up native model migration, not a silent
new session. The official launcher's `--resume <uuid> --model <selector>` plus
native RPC model selection can perform such a one-time migration without
rewriting prior JSONL records. It is not a transport fallback in cc-connect.

## Context usage in reply footers

The adapter implements `ContextUsageReporter` from standard ACP data only:

- `session/update` with `sessionUpdate: "usage_update"` supplies `used` and
  `size`, the current context occupancy and window. An explicitly reported
  zero is valid; malformed, missing, or foreign-session meters are ignored.
- `session/prompt` response `usage` supplies the turn's `inputTokens`,
  `outputTokens`, `cachedReadTokens`, and `cachedWriteTokens`, stored before
  emitting the final result. They may span several model/tool requests.

A native meter renders as `ctx 15.6% (40960/262144)` in rich-card footers,
including exact token counts and a one-decimal percentage. Turn totals never
become context occupancy: a 1.28M-token turn can leave only 40,960 tokens in the
current context. Neither model catalogs nor character-count estimates are used
to invent missing context/window values. A new prompt clears old metrics so
missing usage cannot reuse another turn's data. Usage notifications never enter
the answer body, including metadata received during historical load.

OMP 18.8.6 emits the native meter at the end of a turn. Do not assume a freshly
loaded session has reported one before another turn runs. Footer visibility
still follows `reply_footer` / `show_context_indicator` and remains off while
streaming. Other agents retain their existing footer representation unless
they explicitly report a known context meter.

The optional local-provider integration test checks actual OMP tool turns,
turn/context accounting, and the same UUID after process restart; no cloud key
is needed:

```bash
CC_OMP_NATIVE_USAGE_TEST=1 go test ./agent/acp \
  -run TestOMPContextUsage_NativeACPAndResume -v
```

## Moving from bridge-owned preferences

Old `workspace_model_overrides` or `workspace_reasoning_overrides` in the project
state require an **explicit offline migration**. Startup refuses them before
starting platform pollers; it does not replay them or silently choose a winner.
CLI-only adapters continue using their own bridge-managed model defaults.

Stop all frontends sharing the profile, back up bridge/native state, and compare
each saved UUID with its old workspace preference. Confirm which selection to
keep, remove only reviewed duplicate fields, and restart. Native JSONL histories
must not be rewritten. The container repository provides `omp-model-state.mjs`
for backed-up inspection and explicit `--keep-native` cleanup. To retain a bridge
choice instead, change the same UUID through official native controls first,
then inspect again. Unavailable archived models remain unchanged and still need
native authorization/migration before they can be loaded.

OMP TUI and ACP are separate frontends: do not open the same UUID concurrently.
Sharing native persisted state enables **sequential** use, not live TUI attachment
to an existing ACP process. Unified role/default menus are a separate UI layer;
OMP 18.8.6 has no ACP role-setting operation.

## Validation

`agent/acp/model_controls_test.go` covers new/load metadata, grouped model
options, opaque config IDs, live switches, model-specific effort changes,
failed/unconfirmed replies, session-scoped notifications, and active-prompt
rejection. Core command/card/workspace tests and a critical user journey cover
history continuity and acknowledgement-before-success.

An opt-in test runs the same adapter against official OMP with an isolated
profile and verifies native GLM/DeepSeek switching, unchanged role defaults,
new-process default inheritance, and cross-process resume preserving the saved
selection even after the role default changes:

```bash
CC_OMP_NATIVE_CONTROL_TEST=1 go test ./agent/acp \
  -run TestOMPModelControls_NativeACPAcrossProcesses -v
```

To verify that a deployment delegates its defaults to OMP, optionally set
`CC_OMP_CONTAINER_CONFIG_TEST` to its mounted `config.toml`. The test applies that
file's ACP constructor preferences, changes the isolated native role default,
and checks behavior on a new conversation and a resumed UUID. An explicit constructor
default affects new conversations only; it must never reset a saved selection.

It needs the native `OLLAMA_CLOUD_API_KEY`, not `OLLAMA_API_KEY`. It makes no LLM
requests and never copies credentials into the test profile. In a container
with a `noexec` `/tmp`, set `TMPDIR` to an executable temporary project directory
and remove it after the test.
