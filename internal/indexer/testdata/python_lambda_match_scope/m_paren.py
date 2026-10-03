from lib import full


def run():
    match (lambda: "paren"):
        case(full):
            return full()
