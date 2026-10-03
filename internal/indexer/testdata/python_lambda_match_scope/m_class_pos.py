from lib import full


class Point:
    __match_args__ = ("a",)

    def __init__(self, a):
        self.a = a


def run():
    match Point(lambda: "positional"):
        case Point(full):
            return full()
