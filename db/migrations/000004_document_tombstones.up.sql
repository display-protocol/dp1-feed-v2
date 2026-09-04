-- Tombstones for deleted documents.
--
-- Owner signatures live inside a document and are public via GET, so anyone who has seen a document can
-- replay it. For POST that means resurrecting a resource its owner deliberately deleted: the old bytes
-- still carry a valid owner signature, and create is intentionally open (so that a feed can mirror a
-- document it does not own). A tombstone retires the id on this feed instead, which works because `id` is
-- inside the signed payload and therefore cannot be forged on a replayed document.
--
-- Consequence: once an owner deletes a resource here, that id can never be re-created on this feed; a
-- genuine re-publish must use a new id.
CREATE TABLE IF NOT EXISTS deleted_documents (
    resource_type TEXT        NOT NULL,
    id            UUID        NOT NULL,
    deleted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (resource_type, id)
);
