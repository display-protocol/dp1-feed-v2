-- Restore ON DELETE RESTRICT on the membership -> playlist foreign keys.
--
-- Reverting reinstates the cross-owner deadlock this migration removed: a third party's group or channel
-- reference will again block the playlist owner's signed DELETE. Rows that CASCADE already removed are
-- not recoverable here; only the constraint is restored.

ALTER TABLE playlist_group_members
    DROP CONSTRAINT playlist_group_members_playlist_id_fkey,
    ADD CONSTRAINT playlist_group_members_playlist_id_fkey
        FOREIGN KEY (playlist_id) REFERENCES playlists (id) ON DELETE RESTRICT;

ALTER TABLE channel_members
    DROP CONSTRAINT channel_members_playlist_id_fkey,
    ADD CONSTRAINT channel_members_playlist_id_fkey
        FOREIGN KEY (playlist_id) REFERENCES playlists (id) ON DELETE RESTRICT;
