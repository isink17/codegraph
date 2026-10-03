import sys

try:
    from fast import speed
except ImportError:
    def speed():
        return "mod.speed"

if sys.platform == "win32":
    def pick():
        return "win"
else:
    def pick():
        return "other"


def a(x):
    if x:
        def h():
            return "a.h"
    return "a"


def b():
    try:
        return h()
    except NameError:
        return "NameError"


def c():
    def inner():
        return "c.inner"
    return inner()


def d():
    return pick()


class S:
    if True:
        def run(self):
            return "S.run"

    def go(self):
        try:
            return run()
        except NameError:
            return "NameError"
