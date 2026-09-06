-- ai_gateway_image_jobs is the per-job idempotency marker for
-- TaskTypeImageGenerate (go/ai-gateway/image_gateway.go's imageGenerateHandler):
-- at most one row per enqueued image-generation Job, keyed by its
-- jobs.JobID (stable across every retry of that Job -- see jobs.Job.ID's
-- own doc comment), recording whether the vendor already answered
-- successfully for it and, once go/storage has durably written the
-- output, what it was written as.
--
-- It is tenant data (docs/internal/04-data-and-tenancy.md's data-domain
-- table), deliberately implementing dbkit.TenantScoped (through the
-- embedded dbkit.TenantModel in go/ai-gateway/image_job_store.go's
-- imageJobRow) exactly like go/storage's own objects table: an image-
-- generation job's marker is meaningless outside the tenant it was
-- enqueued under and must never be visible across one. It is reached only
-- through imageJobRepository, which embeds dbkit.Repository[imageJobRow],
-- and its isolation is proven by tenancytest.AssertIsolated
-- (image_job_store_test.go).
--
-- The row exists to close a real bug (this table's own introducing round):
-- go/jobs retries a Handle call that returned an error, and a failure AFTER
-- the vendor call already succeeded (a go/storage write failure -- the
-- module's own AGENTS.md records the SQLITE_BUSY contention this actually
-- hits in practice) must never let the retry call the vendor a second
-- time, since that both bills the tenant again and produces a second usage
-- record for one logical job. imageGenerateHandler.Handle consults this
-- table FIRST, before ever calling ImageProvider, and claims a row (status
-- "pending") BEFORE calling the vendor, never after -- see
-- go/ai-gateway/image_job_store.go's own doc comment for why that ordering,
-- not a marker written only once the vendor already answered, is what
-- closes both a transient claim-write failure and two overlapping Handle
-- calls for the same job, not merely a sequential retry.
--
-- status is "pending" (claimed, no vendor answer recorded yet -- content/
-- mime/image_count/steps/resolution_tier are all still zero), "generated"
-- (the vendor answered; those columns are its raw answer) or "completed"
-- (the generated image has been durably written to go/storage as
-- output_object_id, and content has been cleared -- once storage holds the
-- bytes, keeping a second copy here serves no purpose). There is
-- deliberately no row at all for "no attempt has reached the vendor yet";
-- absence of a row for a job id IS that state.
--
-- content/mime are the vendor's raw answer, persisted for exactly as long
-- as it takes a retry to redo the storage write without asking the vendor
-- again -- see go/ai-gateway/AGENTS.md for the accepted trade-off of
-- storing image bytes here temporarily, in exchange for closing the
-- ordering window a marker written only after the storage write could not.
--
-- provider records which ImageProviderRegistry name actually answered, so
-- an attempt that skips the vendor call (because status is already
-- "generated") can still log which provider generated the image, without
-- re-resolving a credential it no longer needs.
--
-- image_count/steps/resolution_tier are ImageUsage's own fields, stored as
-- this table's own plain columns rather than a JSON blob (native arrays and
-- JSONB operator filtering are both PostgreSQL-only features this codebase
-- avoids), since ImageUsage is small and fixed-shape.
--
-- output_object_id is empty until status is "completed".
--
-- id is the application-generated jobs.JobID (never gen_random_uuid()),
-- and created_at/updated_at are written by the application, never a
-- database default -- the same dual-dialect discipline every other table
-- in this codebase follows.
CREATE TABLE ai_gateway_image_jobs (
    id                VARCHAR(64)  NOT NULL,
    tenant_id         VARCHAR(64)  NOT NULL,
    status            VARCHAR(16)  NOT NULL,
    provider          VARCHAR(100) NOT NULL DEFAULT '',
    content           BYTEA,
    mime              VARCHAR(255) NOT NULL DEFAULT '',
    image_count       INTEGER      NOT NULL DEFAULT 0,
    steps             INTEGER      NOT NULL DEFAULT 0,
    resolution_tier   VARCHAR(64)  NOT NULL DEFAULT '',
    output_object_id  VARCHAR(64)  NOT NULL DEFAULT '',
    created_at        TIMESTAMP    NOT NULL,
    updated_at        TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);
