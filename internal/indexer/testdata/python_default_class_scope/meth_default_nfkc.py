from lib import full


def run():
    class C:
        full = lambda: "cls"

        def m(self, x=ｆｕｌｌ()):
            return x
    return C().m()
