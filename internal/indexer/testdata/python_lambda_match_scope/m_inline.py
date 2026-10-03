from lib import full


def run(x=lambda: "capture"):
    match x:
        case full: return full()
