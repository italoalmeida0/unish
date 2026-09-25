"""Generate benchmark data (deterministic, same on every platform)."""
import os, random, sys

d = sys.argv[1] if len(sys.argv) > 1 else "benchdata"
os.makedirs(d, exist_ok=True)
os.makedirs(os.path.join(d, "many"), exist_ok=True)

rnd = random.Random(42)

with open(os.path.join(d, "nums.txt"), "w", encoding="ascii") as f:
    f.write("\n".join(str(rnd.randint(0, 10**6)) for _ in range(200_000)))
    f.write("\n")

with open(os.path.join(d, "pairs.txt"), "w", encoding="ascii") as f:
    f.write("\n".join(f"k{rnd.randint(0,9999)} {rnd.randint(0,10**6)}" for _ in range(200_000)))
    f.write("\n")

words = ["aaa", "bbb", "foo", "bar", "xyz", "hello", "world", "quux", "foo", "baz"]
with open(os.path.join(d, "big.txt"), "w", encoding="ascii") as f:
    for _ in range(200_000):
        f.write(" ".join(rnd.choice(words) for _ in range(6)) + "\n")

with open(os.path.join(d, "big.csv"), "w", encoding="ascii") as f:
    for i in range(200_000):
        f.write(f"{rnd.choice(words)},{i%1000},{rnd.choice(words)}\n")

with open(os.path.join(d, "big16.bin"), "wb") as f:
    block = bytes(rnd.getrandbits(8) for _ in range(65536))
    for _ in range(256):  # 16 MiB
        f.write(block)

for i in range(2000):
    with open(os.path.join(d, "many", f"f{i:04d}.txt"), "w", encoding="ascii") as f:
        f.write(f"file {i} content\n")

with open(os.path.join(d, "w2000.txt"), "w", encoding="ascii") as f:
    f.write(" ".join("x%d" % i for i in range(2000)))

print("data ready:", d)
