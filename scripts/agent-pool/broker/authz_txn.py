"""CommitTxn (A1 §A2.3): the single-use write permission that only
authz.revalidate issues. An AST test keeps _issue out of every other module."""

import itertools

_COUNTER = itertools.count(1)


class TxnError(AssertionError):
    """A write was attempted without the live transaction: a defect, never a refusal."""


class CommitTxn:
    __slots__ = ("txn_id", "doc_digest", "ctx_digest", "_live")

    def __init__(self, *_a, **_k):
        raise TypeError("a CommitTxn is issued only by authz.revalidate")

    def require(self):
        if not self._live:
            raise TxnError("CommitTxn is closed")

    def close(self):
        self._live = False

    @property
    def live(self):
        return self._live


def _issue(doc_digest, ctx_digest):
    txn = object.__new__(CommitTxn)
    txn.txn_id = next(_COUNTER)
    txn.doc_digest = doc_digest
    txn.ctx_digest = ctx_digest
    txn._live = True
    return txn
