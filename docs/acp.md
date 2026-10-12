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

## Conversation continuity and scope

Model and reasoning changes retain the native session UUID, bridge history,
and running process. Changes during an active prompt are rejected rather than
interrupting that turn. Workspace model and effort preferences are persisted
separately and applied when reconstructing that workspace's agent. Native
session files also retain their selected model/effort across process restarts.

An explicit resume failure is returned to the user. It does not trigger a new
conversation or erase the saved UUID, including relay sessions. The adapter
uses advertised `session/close` support to flush native state before stopping
its subprocess; close is idempotent.

## OMP 18.8.6

Use the official CLI with `cmd = "omp"`, `args = ["acp"]`,
`auth_method = "agent"`, and ACP `mode = "default"`. Tool approval remains a
separate native setting (`tools.approvalMode: yolo`). Optional ACP constructor
preferences are `model` and `reasoning_effort`; the workspace state overrides
these after successful user selections.

OMP exposes categories `model` and `thought_level`, including its model-specific
`max` selector. The picker controls the conversation model; configure smol,
task, and advisor roles using native OMP `modelRoles`, not a Pi compatibility
shim. Preserve additional roles when replacing that record.

If OMP cannot restore a saved model whose credential is unavailable,
`session/load` fails *before* a model config option can be changed. The bridge
surfaces OMP's `data.details` error and retains the binding. This requires
reauthorization or an explicit, backed-up native model migration, not a silent
new session. The official launcher's `--resume <uuid> --model <selector>` plus
native RPC model selection can perform such a one-time migration without
rewriting prior JSONL records. It is not a transport fallback in cc-connect.

## Validation

`agent/acp/model_controls_test.go` covers new/load metadata, grouped model
options, opaque config IDs, live switches, model-specific effort changes,
failed/unconfirmed replies, session-scoped notifications, and active-prompt
rejection. Core command/card/workspace tests and a critical user journey cover
history continuity and acknowledgement-before-success.

An opt-in test runs the same adapter against official OMP with an isolated
profile and verifies native GLM/DeepSeek switching and cross-process resume:

```bash
CC_OMP_NATIVE_CONTROL_TEST=1 go test ./agent/acp \
  -run TestOMPModelControls_NativeACPAcrossProcesses -v
```

It needs the native `OLLAMA_CLOUD_API_KEY`, not `OLLAMA_API_KEY`. It makes no LLM
requests and never copies credentials into the test profile. In a container
with a `noexec` `/tmp`, set `TMPDIR` to an executable temporary project directory
and remove it after the test.
