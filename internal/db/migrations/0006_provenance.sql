-- M4: provenance and licensing (§4, §9).
--
-- §14 assigns the pack provenance fields, the licence table and the capture form
-- to M4; M1 created packs with identity columns only. This migration adds the
-- rest, and the licence lookup they reference.
--
-- Same conventions: STRICT tables, INTEGER Unix seconds UTC, booleans as 0/1.

-- The licence lookup (§4). Reference data, not user metadata — it is an
-- application enum with flags the licence-risk view (§9) reads, so seeding it in
-- the migration is correct and survives `rebuild-index`. Which licence a pack
-- carries is user metadata and lives on the pack (and in its sidecar), not here.
CREATE TABLE licenses (
    id                   INTEGER PRIMARY KEY,
    -- SPDX identifier where one exists, or a stable slug for the game-asset
    -- licences SPDX does not cover (OGA-BY). Unique so a pack references one row.
    spdx_id              TEXT    NOT NULL UNIQUE,
    name                 TEXT    NOT NULL,
    -- The three flags §9's risk view filters on.
    commercial_ok        INTEGER NOT NULL DEFAULT 0,
    attribution_required INTEGER NOT NULL DEFAULT 0,
    share_alike          INTEGER NOT NULL DEFAULT 0,
    url                  TEXT    NOT NULL DEFAULT ''
) STRICT;

-- A practical seed for a game-asset library. Flags follow each licence's actual
-- terms; the risk view (§9) reads them to surface commercial or attribution
-- problems. Migrations run once, and re-run on a rebuilt database, so a plain
-- INSERT is correct.
INSERT INTO licenses (spdx_id, name, commercial_ok, attribution_required, share_alike, url) VALUES
    ('CC0-1.0',        'Creative Commons Zero v1.0 Universal',       1, 0, 0, 'https://creativecommons.org/publicdomain/zero/1.0/'),
    ('CC-BY-4.0',      'Creative Commons Attribution 4.0',           1, 1, 0, 'https://creativecommons.org/licenses/by/4.0/'),
    ('CC-BY-SA-4.0',   'Creative Commons Attribution-ShareAlike 4.0',1, 1, 1, 'https://creativecommons.org/licenses/by-sa/4.0/'),
    ('CC-BY-NC-4.0',   'Creative Commons Attribution-NonCommercial 4.0', 0, 1, 0, 'https://creativecommons.org/licenses/by-nc/4.0/'),
    ('CC-BY-NC-SA-4.0','Creative Commons Attribution-NonCommercial-ShareAlike 4.0', 0, 1, 1, 'https://creativecommons.org/licenses/by-nc-sa/4.0/'),
    ('OGA-BY-3.0',     'OpenGameArt Attribution 3.0',                1, 1, 0, 'https://static.opengameart.org/OGA-BY-3.0.txt'),
    ('MIT',            'MIT License',                                1, 1, 0, 'https://opensource.org/license/mit/'),
    ('Apache-2.0',     'Apache License 2.0',                         1, 1, 0, 'https://www.apache.org/licenses/LICENSE-2.0'),
    ('GPL-3.0-only',   'GNU General Public License v3.0 only',       1, 1, 1, 'https://www.gnu.org/licenses/gpl-3.0.html'),
    ('Unlicense',      'The Unlicense',                              1, 0, 0, 'https://unlicense.org/'),
    ('Proprietary',    'Proprietary / all rights reserved',          0, 0, 0, '');

-- --- pack provenance columns (§4) ------------------------------------------
--
-- SQLite allows only one ADD COLUMN per statement. Text fields default to '' so
-- a pack row is never NULL-riddled; the truly optional numerics stay NULL.

ALTER TABLE packs ADD COLUMN source_url        TEXT NOT NULL DEFAULT '';
ALTER TABLE packs ADD COLUMN source_site       TEXT NOT NULL DEFAULT '';
ALTER TABLE packs ADD COLUMN source_author     TEXT NOT NULL DEFAULT '';
ALTER TABLE packs ADD COLUMN source_author_url TEXT NOT NULL DEFAULT '';

-- The chosen licence, or NULL when unverified (§9: unlicensed packs are tagged,
-- not blocked). No ON DELETE action: the seeded licences are never deleted.
ALTER TABLE packs ADD COLUMN license_id   INTEGER REFERENCES licenses(id);
ALTER TABLE packs ADD COLUMN license_note TEXT NOT NULL DEFAULT '';

-- Attribution requirement can be asserted on the pack even before a licence row
-- is chosen, and the text is what CREDITS.md (§9) emits.
ALTER TABLE packs ADD COLUMN attribution_required INTEGER NOT NULL DEFAULT 0;
ALTER TABLE packs ADD COLUMN attribution_text     TEXT    NOT NULL DEFAULT '';

-- Acquisition record, so a purchase traces back to a receipt (§9). All optional.
ALTER TABLE packs ADD COLUMN acquired_at      INTEGER;
ALTER TABLE packs ADD COLUMN price_paid_cents INTEGER;
ALTER TABLE packs ADD COLUMN currency         TEXT NOT NULL DEFAULT '';
ALTER TABLE packs ADD COLUMN order_ref        TEXT NOT NULL DEFAULT '';

-- The original archive's identity, retained after extraction (§9) so a delisted
-- bundle is still traceable. NULL size until an ingest sets it.
ALTER TABLE packs ADD COLUMN original_archive_name   TEXT NOT NULL DEFAULT '';
ALTER TABLE packs ADD COLUMN original_archive_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE packs ADD COLUMN original_archive_size   INTEGER;

-- §9: a pack without confirmed licence information is fully usable but flagged.
-- Everything starts needing provenance — including the packs M1 already indexed —
-- and the capture form (§9, M4-4g) is what clears it.
ALTER TABLE packs ADD COLUMN provenance_state TEXT NOT NULL DEFAULT 'needs_provenance'
    CHECK (provenance_state IN ('complete', 'needs_provenance'));

ALTER TABLE packs ADD COLUMN notes TEXT NOT NULL DEFAULT '';

-- The licence-risk view (§9) scans for packs needing attention; this is the index
-- it rides. Partial, because the resolved packs are the ones it never looks at.
CREATE INDEX packs_provenance_idx ON packs(provenance_state) WHERE provenance_state = 'needs_provenance';
