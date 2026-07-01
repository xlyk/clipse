# clipse-agent

Python worker for Clipse. Wraps a per-lane LangGraph graph around Deep Agents
Code (DAC); emits a typed `WorkerResult` (see `../schema/worker-result.schema.json`)
as JSON on stdout. See `../docs/design/2026-07-01-clipse-design.md` for the
full architecture.
