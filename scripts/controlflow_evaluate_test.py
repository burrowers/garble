import unittest

from controlflow_evaluate import blocks, instructions, matching


class EvaluationTest(unittest.TestCase):
    def test_instruction_boundaries(self):
        text = """TEXT main.compute(SB)
 main.go:1 0x1000 83f801 CMPL AX, $0x1
 main.go:1 0x1003 7402 JE 0x1007
 main.go:1 0x1005 31c0 XORL AX, AX
 main.go:1 0x1007 c3 RET
"""
        parsed = instructions(text)
        self.assertEqual(sum(i["size"] for i in parsed), 8)
        self.assertEqual(blocks(parsed), [["CMPL", "JE"], ["XORL"], ["RET"]])

    def test_empty_disassembly_fails(self):
        with self.assertRaises(ValueError):
            instructions("no symbol")

    def test_matching_ignores_labels_and_counts_ties(self):
        functions = [{"function": "secret", "opcodes": ["ADD", "RET"], "blocks": [["ADD", "RET"]]},
                     {"function": "other", "opcodes": ["XOR", "RET"], "blocks": [["XOR", "RET"]]}]
        first = {"seed": 1, "functions": functions}
        second = {"seed": 2, "functions": functions}
        self.assertEqual(matching([first, second])[0]["correct_unique_function_matches"], 2)
        second = {"seed": 2, "functions": [functions[0], functions[0]]}
        self.assertEqual(matching([first, second])[0]["correct_unique_function_matches"], 0)


if __name__ == "__main__":
    unittest.main()
