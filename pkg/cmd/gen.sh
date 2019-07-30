echo "package cmd"
for n in adv.txt nouns.txt adj.txt; do
	b=$(basename $n .txt)
	echo "var ${b}List = []string{"
	sed -e 's/.*/"&",/g' <$n
	echo "}"
done
