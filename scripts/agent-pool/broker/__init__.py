"""O5 protocol core: pure, stdlib-only decision logic for the owner-only
recovery broker, plus a read-only Git worktree observer (fdwalk, gitraw,
indexverify). Only the observer reads files, and it never writes or runs a
process; the core performs no file, network or process I/O. Nothing imports
the package outside its tests, and it enables no privileged operation.
Normative contract: O5-DESIGN.r4 with addenda A1 and R5-1; see CONTRACT.md."""
