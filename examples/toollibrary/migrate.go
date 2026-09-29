package toollibrary

// Generation 3 requires every library to have a name, since the tenant list (Library.All) sorts by
// it. Generation 2 didn't, so the migration job must be told what to call a library without one:
// its slug, or else its id.
func init() {
	MigrateLibrary = func(old LibraryG2) (Library, error) {
		lib := AutoMigrateLibrary(old)
		switch {
		case lib.Name != "":
		case old.Slug != "":
			lib.Name = old.Slug
		default:
			lib.Name = old.LibraryID
		}
		return lib, nil
	}
}
