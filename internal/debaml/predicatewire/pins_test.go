//go:build integration

package predicatewire

// pwProjectSHA256 pins the rendered source of every isolated project, beside the
// checked-in goldens under testdata/projects.
//
// It is a SECOND pin on purpose. The golden byte-comparison already fails on any table
// change, but a regeneration rewrites the golden — so without a hash that has to be
// updated by hand, a project could be regenerated in the same commit that changed it and
// the diff would look like an intended edit. [TestPredicateWireProjectDrift] requires
// this map to cover exactly the project table, so a new project cannot arrive unpinned
// either.
var pwProjectSHA256 = map[string]string{
	"bound_exact_above":     "536c0e82d335db0531cd5f6301e92d10c4ef331ce4187dac01e4a0e43d6d660e",
	"bound_exact_at":        "c5811ab1ab744509821f10fd3c8310911b490904cfe95448780b3b6f2375ea56",
	"bound_exact_below":     "f756982dc5a01e25335dd5eec9eafeca391f376ad6dbb15a1cdb8dcdae0887fd",
	"bound_i64max":          "66a384ba94bb189089ea64216fbffc322abbb8f39f3b9095d358b48b594fd88b",
	"bound_i64max_below":    "a765de8d60d908eb4ed7fdaf4d533872179b7899822a2088222ea99a063b3fa0",
	"bound_i64min":          "a020368dee6b383fff01c9702efa8591cf5e849875a9cd39985aff76a0daaa91",
	"bound_i64min_above":    "37fdf697074df5e889df971ca3bec735b22cac5c13a62820f13c931c548de63b",
	"bound_neg_exact_above": "b85b81dd2361caddfab7823ac9d4787d8e4639edd691f14e91e4b6be582a7293",
	"bound_neg_exact_at":    "d100a2e0ea13821c11b0210841bc9ce037e836a82a0cf52efac3a0beeb4831d5",
	"bound_neg_exact_below": "aa9646a977f027e3e17a74bc17e7a0af32c4d281289dad0562a26294d0ff513d",
	"bound_neg_one":         "00ada11ff3d4c92752b095f4c64568a13476d1238fbc82eaf9c095a15e5a2ed4",
	"bound_pos_one":         "d4a5d79af85d142ea3cfbee0f9f081f37f1d6d4f487e0489986fe5a9e07a8ba5",
	"bound_zero":            "3f34ce0e5fb95573a15f3cb1be7a3a65d544dfd526ca65eb149175d9d8d6d88d",
	"exprtext":              "38e6c0050e2daf9bd4c62948c518af57628dc3de4d2bc5874439db86ec94e46f",
	"lit_float":             "bca1b657ccb9673d3293396e530e7b10bc8a55e396e4d5f683fe2569d9894b5a",
	"lit_leading_zeros":     "8b129ad54ba27eb9d4ff58a62d1a8d6d097d55b87d815637a80392e76bc6af1c",
	"lit_overflow":          "df4f7cf94843d40c004afdc440f64db2433d5c5cbff6307ff281ca2968ecf212",
	"lit_plus5":             "1d084ace34d259aef19842d32e404fa17e44a9398840e19fb9697b96da06bcb7",
	"lit_underscore":        "dcc520997566ab7098a6c5341e3096f349d01fa7990b2bd5cb404bef8802cb48",
	"op_eq":                 "d6b37014bf55f2950e2e47daa8131cebed83f239aa27f92125c3b6bc21ab058b",
	"op_ge":                 "d8d1370a2674e13a571cbc446b8348240c0f1a87d72e47e3ec3642586811e789",
	"op_gt":                 "02d525877e0b3303665e1162ce7bc9ccfdf3158c422fd423328ccbcbea05dff9",
	"op_le":                 "7d706cf11556b264d18a7739c5170dc78ae00c961f17bf4d23e8d5351c34be24",
	"op_lt":                 "925cfa1f422d8aeb1a53d488b0c6778664e38a20fbe3d85d1c625fdbfcd6dd8b",
	"op_ne":                 "3281312944fdaa286d944a6bcb2f0c76df399e482983766daab8e32cfd696982",
	"res_assert_then_check": "82965dabb57a0ed017ada0a5520fa11bd0095f242e2f22fdd7ec33b00798624c",
	"res_check_then_assert": "ab6bdc1f6ce0a82e3d1d90e41cd0b8dabdc549dca0f6b0ceec46c356822c67f5",
	"res_duplicate_labels":  "6ada9682ee0e63e962c50c609ee5eaf52fedaece7f349fa0c9ba6f7ff5c160a5",
	"res_three_checks":      "a610e27fb88539b83ba83e50fca0c224b953a61c65cd7b8a73157f268d7477ad",
	"res_two_asserts":       "bed08979ac879a6fe5b8356d16d56c9452703da1d5c4d1789f2c98c7be755a8a",
	"res_two_checks":        "843c2e259fc860a562229019a7e8a743f798ae00e15758cd6c16ecce4f554353",
	"toplevel":              "4c12f2c0f4add5b67dbd177fbd0216bc51d1f12a643dfff48fe383cbae98ee5d",
}
