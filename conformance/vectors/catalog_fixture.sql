-- What the cases of catalog.json start from: two disks, two files, one chunk row, one operation,
-- one trash row and one job with one chunk. See conformance/vectors/README.md.
INSERT INTO disks(id, fs_uuid, fs_type, mount_path, root, workdir, role, position, tags, state) VALUES
  ('d1', '3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b', 'ext4', '/mnt/d1', 'media', 'Atoll', 'data', 0, '[]', 'online'),
  ('d2', '01D9F3A2B4C5E6F7', 'ntfs', '/mnt/d2', 'media', 'Atoll', 'data', 1, '["usb"]', 'standby');

INSERT INTO files(id, lpath, lpath_key, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns) VALUES
  (1, x'612F62', 'A/B', 10, 5, 1, 'deferred', 'ok', 1, 1),
  (2, x'63', 'C', 20, 5, 1, 'deferred', 'ok', 1, 1);

INSERT INTO file_chunks(file_id, chunks) VALUES
  (1, zeroblob(32));

INSERT INTO copies(file_id, disk_id, created_by, first_seen_ns, state) VALUES
  (1, 'd1', 'user', 1, 'ok');

INSERT INTO ops(id, disk_id, kind, path, started_ns, state) VALUES
  (7, 'd1', 'trash', x'612F62', 1, 'done');

INSERT INTO trash(id, lpath, disk_id, trash_path, op_id, reason, deleted_ns, expires_ns) VALUES
  (1, x'612F62', 'd1', x'74', 7, 'deleted', 1, 2);

INSERT INTO jobs(id, kind, file_id, state) VALUES
  (1, 'replicate', 2, 'queued');

INSERT INTO job_chunks(job_id, idx, hash) VALUES
  (1, 0, zeroblob(32));
