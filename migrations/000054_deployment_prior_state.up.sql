-- The app's state when a deploy was started, so a deploy that stops before it
-- touches the runtime can put it back (R-146, design 05 §1.2).
--
-- A column rather than something the starter hands the runner, because since
-- 000050 a deploy runs from the queue, on whichever replica claims it: the
-- request that started it, and anything it knew, is gone by then.
ALTER TABLE deployments ADD COLUMN prior_state text;
