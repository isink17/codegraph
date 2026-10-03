from lib import full


def run():
    class C:
        def full():
            return "classdef"
        g = full()
    return C.g
