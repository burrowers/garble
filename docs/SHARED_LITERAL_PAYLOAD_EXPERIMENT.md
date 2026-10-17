# Shared literal payload experiment

This independent experiment starts at 106a652 and addresses only encoded data
storage, not decoder reuse. Each literal occurrence still generates its own
indirectly dispatched decoder and, for strings, its own string-casting helper.
No decoded value is cached.

## Scope and representation

Only the blob family participates. In production this means literals larger than
`internal/literals.MaxSize` (2048 bytes), plus blob-selected embed values below the
minimum size. The five existing per-byte algorithms are unchanged. Selection is
not redirected to blob merely to make a repeat eligible. Package initializers
and closures owned by those initializers are unchanged.

Each original function declaration owns a transformation-time table. Its nested
closures use that table, but other functions, methods, init functions and files
never consult it. Equal input bytes share one canonical XOR ciphertext and key.
String junk bytes and their trim offset are also canonical within that owner;
strings do not generally share payloads with byte literals because of that junk.

Ciphertext is emitted as an immutable Go string constant, not a variable or a
shared dispatcher root. References from independent decoders allow the compiler
to use the same read-only string backing storage. Constants add no dynamic
initializer and cannot retain other owners' decoders. Existing function-owned
proxy trees continue to hide every decoder call.

Every site scrambles a fresh copy of the canonical 32-byte key with that site's
external key parameters using the existing external-key expression generator.
These arguments actually reconstruct key bytes needed by the XOR loop. The
shared ciphertext excludes the blob header and embedded clear key. Decoders
allocate a fresh writable byte slice, decode in place, and clamp its capacity to
the input length. Existing string trimming, slice conversion and array-copy
paths retain their result semantics.

## Tradeoffs

Canonical ciphertext makes repeated values within an owner directly correlatable.
Canonical string padding also reveals that relationship. Independent key
expressions and decoder bodies do not remove this correlation or make XOR
cryptographically secure; this remains obfuscation. The canonical key can be
recovered from any one site's decoder and arguments.

Unique large literals now pay for externally mixed key expressions; the experiment
may increase text size or runtime cost unless backing-data savings outweigh that
overhead. Ordinary repeated short/medium strings see no payload savings. No
whole-binary size claim is made here; compare this exact branch against its exact
base with fixed seeds and identical realistic programs before publication.

## Focused verification

`TestSharedPayload` checks three owner-private constants for five blob sites,
including three independent decoder functions referring to the same constant in
one owner and its closure. Other owners use different constants.
`TestSharedPayloadMutable` checks one constant and six distinct decoders across
slice, pointer-to-slice and pointer-to-array occurrences. Generated programs run
with binary input, mutation independence and exact-capacity assertions. Each
canonical ciphertext is also required to occur exactly once in the built binary.
Perturbing
the external arguments must reach a runtime content assertion, proving those
arguments are not decorative. The remaining literal unit/fuzz-seed tests and vet
are run without a dependency-heavy integration or benchmark build.
