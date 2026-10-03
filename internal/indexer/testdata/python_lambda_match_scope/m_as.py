from lib import full


def run():
    match lambda: "as":
        case _ as full:
            return full()
