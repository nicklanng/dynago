# Concurrency and document versions

Two things can go wrong when two writers touch the same item:

1. **Inside one write.** A read-modify-write reads the item, computes counter changes, and writes,
   while someone else changes the item in between. dynago always prevents this; you don't need to
   do anything.
2. **Across requests.** A user opens an edit form, someone else saves, and then the user saves,
   silently overwriting the other change (a *lost update*). dynago prevents this when you pass
   back the version the user saw.

## What dynago always does

Every item has a revision (`_rev`) that changes on every write. A read-first write is conditioned
on the revision it read, so it cannot overwrite a change it didn't see. If it loses the race, it
reads again and retries (8 attempts over about half a second by default, set with
`dynago.SetRetries`, then `dynago.ErrConflict`). Counters, claims and copies are therefore always
consistent with the item, without any extra code.

Plain field updates without a version are last-writer-wins: if two requests rename a tool at the
same time, the later rename stands. For most background writes that is what you want.

## Versions: preventing lost updates

Every entity the store returns remembers the version it was read at. Nobody needs to look at it;
you pass it back to a write in one of two ways.

### Same request: `dynago.From(entity)`

```go
loan, err := st.Loans.Get(ctx, key)
if err != nil { return err }
if !mayExtend(loan) { return errNotAllowed }        // your business rules, on the loaded entity

err = st.Loans.Extend(ctx, key, toollibrary.LoanExtend{DueAt: loan.DueAt.Add(week)}, dynago.From(loan))
```

- The write fails with `dynago.ErrVersionMismatch` if the item changed after you loaded it, so the
  decision you made on `loan` still holds when it commits.
- **It skips the write's own read.** A write that maintains counters normally reads the item first;
  with `From`, it uses the entity you pass as the "before" state, which saves a round trip.
- The entity must be one the store returned (from a get, a query or a unique lookup) for the same
  key; otherwise the write fails with `dynago.ErrVersionRequired` or `dynago.ErrInvalidKey`.

### Across requests: `dynago.IfVersion(version)`

```go
// GET /tools/{id}
tool, _ := st.Tools.Get(ctx, key)
w.Header().Set("ETag", strconv.Quote(tool.Version())) // ETags are quoted strings (RFC 7232)

// PATCH /tools/{id}
var version string
ifMatch := strings.Trim(strings.TrimPrefix(r.Header.Get("If-Match"), "W/"), `"`)
err := st.Tools.EditDetails(ctx, key, edits, dynago.IfVersion(ifMatch), dynago.ReturnVersion(&version))
if errors.Is(err, dynago.ErrVersionMismatch) {
    http.Error(w, "changed by someone else; reload", http.StatusPreconditionFailed)
    return
}
w.Header().Set("ETag", strconv.Quote(version)) // the new version, without reading the tool again
```

`Version()` is an opaque, URL-safe string. The browser only echoes it back. An empty version means
no check, so a missing `If-Match` header passes straight through (unless the write requires one).
`dynago.ReturnVersion(&s)` hands back the version after the write, and a create leaves the new
version on the entity itself (`e.Version()`), so handlers never need a second read to set an ETag.

- A write that changes nothing derived stays one conditional UpdateItem.
- A read-first write still reads once to compute its changes, but fails instead of retrying if the
  version doesn't match.
- A read-free write (such as `Retire`) given `dynago.ReturnVersion` or `dynago.From` takes the
  read-first path, so it costs a read: both need the stored item.

### Requiring a version

Versioning is optional by default. Mark writes where a lost update would be a bug (human edits of
shared records) as required:

```yaml
EditDetails: { patch: [name, manual, tags], versioned: required }
```

Calling it without `From` or `IfVersion` then fails with `dynago.ErrVersionRequired`, and the model
document shows the policy for every write.

## What a mismatch means

`dynago.ErrVersionMismatch` is never retried by dynago: retrying would overwrite exactly the change
the version exists to protect. Reload the entity and either redo the operation (if it doesn't depend
on what changed), merge, or tell the user.

## Caveat: all writes change the version

Every write changes the version, including background ones: a nightly job that updates every tool's
usage statistics will make any tool edit form left open overnight conflict. If that happens on a
real entity, the planned fix is marking such writes as not changing the user-facing version.

## Other guarantees

- **Delete and re-create.** Revisions start at a random value, so an item deleted and re-created
  between your read and your write is detected as a change, never mistaken for the one you read.
- **Rolling deploys.** On every write path, an old instance refuses to change an item a newer
  instance wrote (`dynago.ErrNewerSchema`), so it cannot drop fields or skip counter changes it
  doesn't know about.
- **SDK retries.** Transactions carry an idempotency token, and a single-item create recognises its
  own item (by its random revision) if the SDK retries it after it succeeded. So the AWS SDK's
  automatic retries can neither apply a write twice nor turn a success into "already exists".
- **Optional updates.** `patch:` fields are pointers: fields a caller didn't send are left alone, so
  two people editing different fields of the same record don't overwrite each other's changes
  (unless the write requires a version, in which case the second is told to reload).
