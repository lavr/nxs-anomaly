-- Index what the insights screen counts.
--
-- The screen read nxs_anomaly_alert_groups in full twice per request: once for
-- the status and severity tiles, once for the trend of the last days. On a
-- sandbox with 210,000 groups (289 MB) that was 0.6 s, and under forty readers
-- the slowest read the API served. Status and severity ride along in this
-- index, so both are answered from it — a few megabytes instead of the wide
-- rows. created_at never changes and the two included columns are already
-- indexed elsewhere, so updates cost no more than before.

CREATE INDEX IF NOT EXISTS nxs_anomaly_alert_groups_insights_idx
    ON nxs_anomaly_alert_groups (created_at) INCLUDE (status, severity);
