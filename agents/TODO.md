# TODO

Build order per plan: 1 → 2 → 3 → 6 → 5 → 8 → 7 → 9 → 10 → 11 → 4 → 12,
then the flake's packages and NixOS/home-manager modules, then verification.

- [x] Block 1: skeleton + config
- [x] Block 2: data model + store
- [x] Block 3: provider interface + ytmusic
- [x] Block 6: media manager
- [x] Block 5: aggregation + matching
- [x] Block 8: HTTP API + event stream
- [x] Block 7: accounts, favorites, provider ranking
- [x] Block 9: listen together
- [x] Block 10: public Go SDK
- [x] Block 11: CLI
- [x] Block 4: Spotify provider (live check needs real credentials)
- [x] Block 12: hardening + docs
- [x] Nix flake: packages, apps, service module, CLI modules (NixOS + HM)
- [x] Verification: go test, nix build/flake check, curl sweep (57/57)

Open, both needing input from Kyle:

- Spotify has only been exercised against a fake Web API; point a real
  clientId/clientSecret at it and re-run `go test ./internal/provider/...`.
- The NixOS/HM modules evaluate and build, but nothing wires them into the
  system flake yet; that is a `clan machines update <name>` away, by his call.
