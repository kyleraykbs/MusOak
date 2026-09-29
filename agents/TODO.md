# TODO

Build order per plan: 1 → 2 → 3 → 6 → 5 → 8 → 7 → 9 → 10 → 11 → 4 → 12,
then NixOS/HM modules, then full verification.

- [ ] Block 1: skeleton + config
- [ ] Block 2: data model + store
- [ ] Block 3: provider interface + ytmusic
- [ ] Block 6: media manager
- [ ] Block 5: aggregation + matching
- [ ] Block 8: HTTP API + event stream
- [ ] Block 7: accounts, favorites, provider ranking
- [ ] Block 9: listen together
- [ ] Block 10: public Go SDK
- [ ] Block 11: CLI
- [ ] Block 4: Spotify provider
- [ ] Block 12: hardening + docs
- [ ] Nix flake: service module + CLI modules (NixOS + HM)
- [ ] Verification: go test, nix flake check, smoke run
