from lib import full


def run():
    match [lambda: "bracket"]:
        case[full]:
            return full()
