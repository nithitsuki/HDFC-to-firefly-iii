# Sample emails

Put real HDFC alert emails here. These files calibrate the parser and test it
again after a change.

**CAUTION:** These files do not go into the repository. The files contain your
account digits, the amounts, and the names of the payees. The `.gitignore` file
excludes them.

## Get a sample from Gmail

Do these steps for each message:

1. Open the message in Gmail on a computer.
2. Click the three-dot **More** menu at the top right of the message.
3. Click **Show original**. A new tab opens with the raw message.
4. Click **Download Original**. Gmail saves a `.eml` file.
5. Move the file to this directory.

If you do not want a file, click **Copy to clipboard** and put the text in a new
file.

## Which messages to get

Get one message for each different wording. You do not need one message for each
transaction.

| File name | Purpose |
| --- | --- |
| `upi-debit.eml` | The usual message |
| `upi-credit.eml` | Money that comes in |
| `balance-notice.eml` | A balance notice |
| `card.eml` | A card alert, if you get one |

One example of each type is sufficient.

## Test the parser

```sh
go test ./internal/hdfcmail/ -run TestSamples -v
```

If a sample has no `.json` file, the test shows the result of the parser. Read
the result. If the result is correct, save the result as a `.json` file with the
same name as the `.eml` file:

```json
{
  "kind": "upi_debit",
  "amount_minor": 10000,
  "date": "2026-01-05",
  "last4": "1234",
  "payee": "Example Merchant"
}
```

The amount is in minor units. One rupee is 100 minor units. Thus `10000` is
100.00 rupees.

If the message must never go to the ledger, write this `.json` file:

```json
{
  "skip": true
}
```

## Put a sample in the repository

Do not put a real email in the repository. If you want a test that operates on
a build server, clean a copy of the email first:

1. Change the amount.
2. Change the account digits.
3. Change the reference number.
4. Change the names of the payees.

Keep the words and the structure of the message the same. The parser tests the
words and the structure.
