BEGIN TRANSACTION;
CREATE TABLE entries (seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT UNIQUE NOT NULL, body TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1,
  retirement TEXT);
INSERT INTO "entries" VALUES(1,'o-96a9fd79fc6751319006ead6','{"seq":0,"id":"o-96a9fd79fc6751319006ead6","kind":"observation","timestamp":"2026-09-06T06:40:53Z","importance":"high","text":"Keep SQLite and evidence <&> 🙂 .","support":["s-a0d0816f936aca12e74686b5"],"active":true,"retirement":null}',0,'{"id":"o-96a9fd79fc6751319006ead6","reason":"Preserved in reflection.","replacement_ids":["r-4cd43206803f277e07be3336"]}');
INSERT INTO "entries" VALUES(2,'r-4cd43206803f277e07be3336','{"seq":0,"id":"r-4cd43206803f277e07be3336","kind":"reflection","timestamp":"2026-09-06T06:40:53Z","importance":"medium","text":"Preserve local evidence.","support":["o-96a9fd79fc6751319006ead6"],"active":true,"retirement":null}',1,NULL);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO "meta" VALUES('session','legacy <&> 🙂  literal \u2028');
INSERT INTO "meta" VALUES('version','1');
INSERT INTO "meta" VALUES('cursor','1');
CREATE TABLE sources (seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT UNIQUE NOT NULL, body TEXT NOT NULL);
INSERT INTO "sources" VALUES(1,'s-a0d0816f936aca12e74686b5','{"seq":0,"id":"s-a0d0816f936aca12e74686b5","kind":"user","timestamp":"2026-09-06T06:40:53Z","text":"Keep SQLite and exact evidence <&> 🙂   literal \\u2028.","truncated":false}');
DELETE FROM "sqlite_sequence";
INSERT INTO "sqlite_sequence" VALUES('sources',1);
INSERT INTO "sqlite_sequence" VALUES('entries',2);
COMMIT;
