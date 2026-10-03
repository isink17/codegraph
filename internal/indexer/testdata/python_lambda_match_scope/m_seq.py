from lib import full


def run():
    match [lambda: "seq"]:
        case [full]:
            return full()
