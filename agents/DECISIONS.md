# Decisions

## [2026-09-28] Module path
- Decision: `codeberg.org/kyleraykbs/prismusic` (same as v1)
- Why: v1 go.mod uses it; keeps the public SDK import path stable across versions.
- Reversible: no (touches every import)

## [2026-09-28] Naming: Prismusic inside `musoak/` dir
- Decision: keep Prismusic naming (binaries `prism`, `prismusicd`, config dir `prismusic`) even though the repo lives in `projects/musoak`.
- Why: plan is titled Prismusic v2; nothing references the directory name.
- Reversible: yes

## [2026-09-28] isrc column on variants
- Decision: add `isrc` to `variants` even though the plan's table list omits it.
- Why: ISRC is match signal #1 (Block 5) and Block 4 populates it; it must live on the provider-side row.
- Reversible: yes
