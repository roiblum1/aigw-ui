-- The market price of a model (internal/market). models.market_id is the
-- OpenRouter entry an operator chose for the model: NULL follows the
-- annotation aigw-ui.io/market-id and the automatic match, '' is no market
-- price. model_deployments.market_id is that annotation as each deployment
-- declares it. model_prices.market is "<entry>@<snapshot date>" for prices
-- taken unchanged from the market, so the page can say when it moved.
ALTER TABLE models ADD COLUMN market_id text;
ALTER TABLE model_deployments ADD COLUMN market_id text NOT NULL DEFAULT '';
ALTER TABLE model_prices ADD COLUMN market text NOT NULL DEFAULT '';
