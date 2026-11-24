// Package names generates human-readable sandbox names in the style of
// haikunator: adjective-noun-1234.
package names

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

var adjectives = []string{
	"agile", "amber", "ancient", "antique", "autumn", "azure", "billowing", "bitter",
	"blazing", "blue", "bold", "brave", "breezy", "bright", "bronze", "busy",
	"calm", "chilly", "clever", "cold", "cool", "crimson", "crystal", "curious",
	"damp", "dancing", "dark", "dawn", "deep", "delicate", "drifting", "dry",
	"dusty", "eager", "early", "empty", "endless", "faded", "faint", "falling",
	"fancy", "fierce", "floating", "floral", "foggy", "fragrant", "freezing", "fresh",
	"frosty", "frozen", "gentle", "golden", "green", "grey", "happy", "hidden",
	"hollow", "humble", "hungry", "icy", "ivory", "jolly", "joyful", "kind",
	"late", "light", "lingering", "little", "lively", "lonely", "long", "loud",
	"lucky", "mellow", "merry", "misty", "moonlit", "morning", "mossy", "muddy",
	"mute", "nameless", "narrow", "new", "noble", "old", "orange", "pale",
	"patient", "peaceful", "pink", "plain", "polished", "proud", "purple", "quick",
	"quiet", "rainy", "rapid", "red", "restless", "ripe", "rising", "roaring",
	"rosy", "rough", "rustic", "sacred", "sandy", "sapphire", "scarlet", "secret",
	"serene", "sharp", "shining", "shy", "silent", "silver", "sleepy", "slow",
	"small", "smooth", "snowy", "soft", "solitary", "sparkling", "spring", "still",
	"summer", "sunlit", "sweet", "tall", "tender", "thick", "tiny", "tranquil",
	"twilight", "velvet", "violet", "wandering", "warm", "weathered", "white", "wide",
	"wild", "winter", "wispy", "yellow", "young",
}

var nouns = []string{
	"acorn", "aurora", "bark", "bay", "beach", "bird", "blossom", "branch",
	"breeze", "brook", "canyon", "cave", "cedar", "cliff", "cloud", "coast",
	"comet", "creek", "crescent", "crystal", "darkness", "dawn", "dew", "dream",
	"dust", "dune", "dusk", "earth", "ember", "evening", "feather", "fern",
	"field", "fire", "firefly", "fjord", "flame", "flint", "flower", "fog",
	"forest", "frost", "galaxy", "garden", "glacier", "glade", "grass", "grove",
	"harbor", "haze", "hill", "island", "jungle", "lagoon", "lake", "leaf",
	"meadow", "meteor", "mist", "moon", "morning", "moss", "mountain", "night",
	"ocean", "orchid", "paper", "peak", "pebble", "pine", "planet", "pond",
	"prairie", "quartz", "rain", "ravine", "reef", "ridge", "river", "rock",
	"rose", "sand", "sea", "seed", "shadow", "shore", "silence", "sky",
	"skyline", "smoke", "snow", "sound", "spring", "spruce", "star", "stone",
	"storm", "stream", "summit", "sun", "sunset", "surf", "swamp", "thicket",
	"thunder", "tide", "trail", "tree", "tulip", "tundra", "valley", "vine",
	"volcano", "water", "waterfall", "wave", "wildflower", "willow", "wind", "wing",
	"wood", "woodland", "woods",
}

func pick(list []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return "", err
	}
	return list[n.Int64()], nil
}

// Generate returns a new name such as "quiet-forest-4821".
func Generate() (string, error) {
	adjective, err := pick(adjectives)
	if err != nil {
		return "", fmt.Errorf("names: adjective: %w", err)
	}
	noun, err := pick(nouns)
	if err != nil {
		return "", fmt.Errorf("names: noun: %w", err)
	}
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", fmt.Errorf("names: number: %w", err)
	}
	return fmt.Sprintf("%s-%s-%04d", adjective, noun, n.Int64()), nil
}
