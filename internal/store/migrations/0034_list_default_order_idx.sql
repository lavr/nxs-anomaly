-- Index the default order of the three time-ordered listings.
--
-- A listing of alerts, alert groups or notifications is ordered by its time
-- column, newest first, ties broken by id (store.defaultSort, orderClause):
-- "ORDER BY received_at DESC NULLS LAST, id DESC". No index matched that order
-- — the existing (integration_id, received_at DESC) one leads with another
-- column and sorts nulls first — so every page sorted the whole table: a
-- third of a second for one page of a million alerts, seconds under forty
-- readers. One index per listing, in exactly the listing's order; the alerts
-- one costs a little on ingest, which is why there is only one.

CREATE INDEX IF NOT EXISTS nxs_anomaly_alerts_list_order_idx
    ON nxs_anomaly_alerts (received_at DESC NULLS LAST, id DESC);

CREATE INDEX IF NOT EXISTS nxs_anomaly_alert_groups_list_order_idx
    ON nxs_anomaly_alert_groups (last_received_at DESC NULLS LAST, id DESC);

CREATE INDEX IF NOT EXISTS nxs_anomaly_notifications_list_order_idx
    ON nxs_anomaly_notifications (created_at DESC NULLS LAST, id DESC);
