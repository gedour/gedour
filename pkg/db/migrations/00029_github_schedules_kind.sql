-- The github_schedules watch kind joins the kind CHECK.
-- +goose Up
ALTER TABLE watches DROP CONSTRAINT watches_kind_check;
ALTER TABLE watches ADD CONSTRAINT watches_kind_check
    CHECK (kind IN ('github_repo', 'github_pr', 'github_issue', 'github_discussion', 'github_schedules', 'gitlab_release', 'gitlab_mr', 'oci_tags'));

-- +goose Down
-- No DELETE: a github_schedules watch left fails the re-added CHECK and aborts the down, keeping the user's data.
ALTER TABLE watches DROP CONSTRAINT watches_kind_check;
ALTER TABLE watches ADD CONSTRAINT watches_kind_check
    CHECK (kind IN ('github_repo', 'github_pr', 'github_issue', 'github_discussion', 'gitlab_release', 'gitlab_mr', 'oci_tags'));
