-- P22.21: syntax-proven Java declaration staticness.
-- NULL means historical or fallback evidence did not prove the modifier.
ALTER TABLE symbols ADD COLUMN is_static INTEGER;
