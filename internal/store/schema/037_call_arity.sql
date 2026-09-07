-- P22.42: syntax-proven callable and callsite arity.
-- arity_max = -1 means an unbounded final params array; NULL means unknown.
ALTER TABLE symbols ADD COLUMN arity_min INTEGER;
ALTER TABLE symbols ADD COLUMN arity_max INTEGER;
ALTER TABLE edges ADD COLUMN call_arity INTEGER;
