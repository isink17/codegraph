from lib import full


class P:
    def __init__(self, a):
        self.a = a


def run():
    match P(lambda: "keyword"):
        case P(a=full):
            return full()
