# Hermes worker 守则：只看服务端下发版本

本文件不含任何规则。Hermes 的 worker 守则只有一个来源：服务端按你的类型和身份渲染的版本
（和 Muse 等其他类型不同，不要混用）。

- 首次接入：用管理员在控制台生成的 onboarding prompt。
- 已接入：`GET <RELAY_URL>/prompts/current`（带你的 token），全文存为
  `~/workspace/task-relay/prompt-current.md`，以它为准。
- 收到 `prompt_update` 时：按 prompt-current.md 里「指令更新」一节整份替换，并清理 memory 中冲突的旧规则。

维护约定：唯一事实来源是 `internal/prompts/templates/hermes.tmpl`。不要在这里复制规则正文，
以前的快照停在旧版本，和服务端版本冲突过（`prompts_test` 会拦截规则正文回流）。
