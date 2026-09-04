-- Membership rows follow the playlist they point at, instead of blocking its deletion.
--
-- Why this changes: creation is open, so ANY caller can self-sign a group or channel referencing an
-- already-stored playlist they do not own. Under ON DELETE RESTRICT that reference vetoed the playlist
-- owner's own signed DELETE, and the documented remedy — remove the references — was impossible for them,
-- because only the referencing document's owner may edit or delete it. A third party could therefore keep
-- someone else's playlist alive indefinitely, defeating owner-controlled deletion and the replay-removal
-- that tombstoning exists to provide.
--
-- Why CASCADE is safe here: these rows are a derived index over the referencing documents, not content
-- anyone owns. A group or channel is read back from its stored signed document (see GetPlaylistGroup /
-- GetChannel, which select `body`), never reassembled from membership rows; those rows serve only the
-- `?playlist-group=` / `?channel=` list filters. So deleting them leaves every third-party document
-- byte-for-byte intact and still verifying, and only stops a filter from returning a playlist that no
-- longer exists — which is the correct answer once it is gone.
--
-- The referencing document keeps listing the deleted playlist's URI. That is deliberate: the document is
-- signed and immutable, so the feed must not edit it. A later PUT of that group/channel will fail to
-- resolve the tombstoned reference, which correctly forces the owner to publish an updated document.

-- Added NOT VALID, then validated separately. Only the ON DELETE action changes, so every existing row
-- already satisfies the constraint; a plain ADD CONSTRAINT would still scan the whole membership table
-- while holding ACCESS EXCLUSIVE, blocking reads and writes for no benefit. NOT VALID skips that scan,
-- and VALIDATE CONSTRAINT takes only SHARE UPDATE EXCLUSIVE, so concurrent traffic keeps running.
-- IF EXISTS on the drop keeps this re-runnable against a database repaired by hand.

ALTER TABLE playlist_group_members
    DROP CONSTRAINT IF EXISTS playlist_group_members_playlist_id_fkey,
    ADD CONSTRAINT playlist_group_members_playlist_id_fkey
        FOREIGN KEY (playlist_id) REFERENCES playlists (id) ON DELETE CASCADE NOT VALID;
ALTER TABLE playlist_group_members VALIDATE CONSTRAINT playlist_group_members_playlist_id_fkey;

ALTER TABLE channel_members
    DROP CONSTRAINT IF EXISTS channel_members_playlist_id_fkey,
    ADD CONSTRAINT channel_members_playlist_id_fkey
        FOREIGN KEY (playlist_id) REFERENCES playlists (id) ON DELETE CASCADE NOT VALID;
ALTER TABLE channel_members VALIDATE CONSTRAINT channel_members_playlist_id_fkey;
