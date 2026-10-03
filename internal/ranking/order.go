package ranking

import (
	"sort"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// OrderVariants puts a track's variants in the caller's effective order. Each
// reserved slot is replaced by the variant it stands for — the caller's own
// upload, or the best-liked upload by somebody else — and skipped when nothing
// fills it, exactly as a provider with no rendition is. Providers keep their
// places, anything the order does not name comes last, and ties break by net
// votes then variant id so the order never wobbles.
func OrderVariants(order []string, variants []store.Variant, uploaders map[uuid.UUID]store.VariantUploader, counts map[uuid.UUID]store.VariantVoteCount, caller uuid.UUID) []store.Variant {
	placed := make([]bool, len(variants))
	out := make([]store.Variant, 0, len(variants))
	place := func(i int) {
		if i < 0 || placed[i] {
			return
		}
		placed[i] = true
		out = append(out, variants[i])
	}
	net := func(i int) int {
		count := counts[variants[i].ID]
		return count.Up - count.Down
	}
	// bestUpload is the highest-voted upload whose uploader matches, the
	// earliest when the tallies tie: the store lists a track's variants oldest
	// first, and that order breaks the tie as it always has.
	bestUpload := func(match func(store.VariantUploader) bool) int {
		best := -1
		for i, variant := range variants {
			uploader, ok := uploaders[variant.ID]
			if !ok || !match(uploader) {
				continue
			}
			if best == -1 || net(i) > net(best) {
				best = i
			}
		}
		return best
	}
	byVotes := func(indices []int) {
		sort.SliceStable(indices, func(a, b int) bool {
			i, j := indices[a], indices[b]
			if net(i) != net(j) {
				return net(i) > net(j)
			}
			return variants[i].ID.String() < variants[j].ID.String()
		})
	}

	for _, name := range order {
		switch name {
		case SlotSelf:
			// A guest has no self: the slot is skipped, never filled with
			// somebody else's upload.
			if caller != uuid.Nil {
				place(bestUpload(func(u store.VariantUploader) bool { return u.UserID == caller }))
			}
		case SlotUploaded:
			place(bestUpload(func(u store.VariantUploader) bool { return caller == uuid.Nil || u.UserID != caller }))
		default:
			group := make([]int, 0)
			for i, variant := range variants {
				if _, isUpload := uploaders[variant.ID]; isUpload {
					continue
				}
				if variant.Provider == name {
					group = append(group, i)
				}
			}
			byVotes(group)
			for _, i := range group {
				place(i)
			}
		}
	}

	// Leftovers are what the order does not name, in the historical grouping:
	// a provider's own rendition before an upload, then the net votes, then
	// the variant id.
	rest := make([]int, 0, len(variants))
	for i := range variants {
		if !placed[i] {
			rest = append(rest, i)
		}
	}
	sort.SliceStable(rest, func(a, b int) bool {
		i, j := rest[a], rest[b]
		officialI := variants[i].Provider != store.UploadProvider
		officialJ := variants[j].Provider != store.UploadProvider
		if officialI != officialJ {
			return officialI
		}
		if net(i) != net(j) {
			return net(i) > net(j)
		}
		return variants[i].ID.String() < variants[j].ID.String()
	})
	for _, i := range rest {
		place(i)
	}
	return out
}
