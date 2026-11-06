-- The github_release watch kind becomes github_repo: rows, the kind CHECK and the literal in saved rule filters.
-- +goose Up
-- Rules before watches: the lock order of AGENTS.md. Literal match only (the three expr quote styles): a filter
-- building the string another way keeps the old value.
UPDATE rules SET filter = replace(replace(replace(filter,
        '"github_release"', '"github_repo"'), '''github_release''', '''github_repo'''), '`github_release`', '`github_repo`')
    WHERE filter LIKE '%github_release%';
-- 00003 declared it inline: watches_kind_check is the name PostgreSQL gave it.
ALTER TABLE watches DROP CONSTRAINT watches_kind_check;
UPDATE watches SET kind = 'github_repo' WHERE kind = 'github_release';
ALTER TABLE watches ADD CONSTRAINT watches_kind_check
    CHECK (kind IN ('github_repo', 'github_pr', 'gitlab_release', 'gitlab_mr', 'oci_tags'));

-- +goose Down
UPDATE rules SET filter = replace(replace(replace(filter,
        '"github_repo"', '"github_release"'), '''github_repo''', '''github_release'''), '`github_repo`', '`github_release`')
    WHERE filter LIKE '%github_repo%';
ALTER TABLE watches DROP CONSTRAINT watches_kind_check;
UPDATE watches SET kind = 'github_release' WHERE kind = 'github_repo';
ALTER TABLE watches ADD CONSTRAINT watches_kind_check
    CHECK (kind IN ('github_release', 'github_pr', 'gitlab_release', 'gitlab_mr', 'oci_tags'));
