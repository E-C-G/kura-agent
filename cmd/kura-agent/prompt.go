package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// stdin est partagé par toutes les saisies : avec une entrée redirigée, un second
// bufio.Reader perdrait ce que le premier a déjà lu.
var stdin = bufio.NewReader(os.Stdin)

func readLine() (string, error) {
	line, err := stdin.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// askSecret pose une question et lit une ligne sans l'afficher quand l'entrée est une
// console (Windows) ; redirigée depuis un fichier ou un tube, la ligne est lue telle quelle.
func askSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	restore := hideInput()
	v, err := readLine()
	if restore != nil {
		restore()
		fmt.Fprintln(os.Stderr)
	}
	return v, err
}

// askAWSKeys demande une paire de clés d'accès et renvoie la valeur du coffre.
func askAWSKeys(name string) (string, error) {
	id, err := askSecret(fmt.Sprintf("%s — identifiant de clé d'accès (Access Key ID) : ", name))
	if err != nil {
		return "", err
	}
	secret, err := askSecret(fmt.Sprintf("%s — clé secrète (Secret Access Key) : ", name))
	if err != nil {
		return "", err
	}
	return joinAWSKeys(id, secret)
}
