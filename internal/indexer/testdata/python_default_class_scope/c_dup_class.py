from lib import full


def run(flag=True):
    if flag:
        class C:
            full = lambda: "cls"
            g = full()
    else:
        class C:
            h = 1
    return C.g
