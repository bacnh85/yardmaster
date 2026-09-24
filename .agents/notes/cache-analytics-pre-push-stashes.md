# Cache-analytics pre-push stash snapshots (recovery anchors)

The 4 WIP stashes the cache-analytics session made BEFORE de8979c was pushed
(2026-09-24 ~07:59 +07) are dangling — no refs/stash entry points at them.
Their content trees are byte-identical to each other and are a strict subset
of de8979c (verified 2026-09-24: every line in stash-but-not-de8979c is a
pre-refactor variant or Cache-tab wiring de8979c itself added — nothing
unique). Recorded for `git gc` safety only; recovery unlikely to be needed.

Stash commits:  97892a6  3a0f69e  089f5c2  f570f49
Content tree (shared): 54a59b3a415fff6c4184b1176ffbfe9c50b6286e
(alt tree ids: 9369a1f, 92e7c2e, 2c94c76 — same content)

Recover with: git stash apply 97892a6
