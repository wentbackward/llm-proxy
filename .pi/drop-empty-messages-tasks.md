# Drop Empty Messages Implementation

- [x] Step 1: Add `DropEmptyContent` config fields (global, backend, route)
- [x] Step 2: Rewrite `sanitizeMessages` → `dropEmptyMessages` with cascade evaluation and filtering
- [x] Step 3: Move the call to post-resolution in `proxyRequest`
- [x] Step 4: Add unit tests
- [x] Step 5: `make check`
