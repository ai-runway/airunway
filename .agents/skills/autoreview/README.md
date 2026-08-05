# Autoreview

This directory vendors the OpenClaw `autoreview` skill with three narrow downstream behavior overrides. Repository-only regression tests are not part of the shipped skill bundle.

## Upstream

- Repository: [`openclaw/agent-skills`](https://github.com/openclaw/agent-skills)
- Source snapshot: [`skills/autoreview` at commit `2a409d348a4bcf6f15e41e9a20efd0b298a32528`](https://github.com/openclaw/agent-skills/tree/2a409d348a4bcf6f15e41e9a20efd0b298a32528/skills/autoreview)
- Commit: [`2a409d348a4bcf6f15e41e9a20efd0b298a32528`](https://github.com/openclaw/agent-skills/commit/2a409d348a4bcf6f15e41e9a20efd0b298a32528) (`docs(skills): add readme-standard house README skill`, August 2, 2026)

The vendored runtime skill matches that snapshot except for the three overrides below, the omission of upstream repository-only test artifacts, and this provenance README.

## Local differences

- Codex defaults to `gpt-5.6-sol` with `max` reasoning instead of upstream's `high` reasoning.
- Claude defaults to `claude-opus-5` with `max` reasoning instead of upstream's `claude-fable-5` default.
- Codex runs with `--ignore-user-config`, so the downstream copy also preserves `openai_base_url` from the external `CODEX_HOME/config.toml` and passes it as an explicit Codex configuration override.

`SKILL.md` documents all three exceptions. No other skill behavior is intentionally changed.

## Packaging

- `scripts/autoreview_test.py` and `tests/` are upstream repository-only regression artifacts and are intentionally omitted from the shipped skill.
- The bundled `--self-test` checks and `scripts/test-review-harness*` smoke harness remain part of the skill.

## Upstream license

OpenClaw's work is used under the MIT License:

```text
MIT License

Copyright (c) 2026 openclaw

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
