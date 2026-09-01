-- derived_items backs migrations_test.go's "derived" fixture module, which
-- declares DependsOn: []string{"base"}. Test fixture only, following the
-- same dual-dialect constraints every real module's migrations must follow.
CREATE TABLE derived_items (
    id        VARCHAR(26) NOT NULL,
    tenant_id VARCHAR(26) NOT NULL,
    base_id   VARCHAR(26) NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
